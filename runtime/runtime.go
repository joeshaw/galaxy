package runtime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strings"
	"time"

	auth "github.com/dotcloud/docker/registry"
	"github.com/fsouza/go-dockerclient"
	"github.com/litl/galaxy/log"
	"github.com/litl/galaxy/registry"
	"github.com/litl/galaxy/utils"
)

var blacklistedContainerId = make(map[string]bool)

type ServiceRuntime struct {
	dockerClient    *docker.Client
	authConfig      *auth.ConfigFile
	shuttleHost     string
	serviceRegistry *registry.ServiceRegistry
}

func NewServiceRuntime(shuttleHost, env, pool, redisHost string) *ServiceRuntime {
	if shuttleHost == "" {
		dockerZero, err := net.InterfaceByName("docker0")
		if err != nil {
			log.Fatalf("ERROR: Unable to find docker0 interface")
		}
		addrs, _ := dockerZero.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				log.Fatalf("ERROR: Unable to parse %s", addr.String())
			}
			if ip.DefaultMask() != nil {
				shuttleHost = ip.String()
				break
			}
		}
	}

	serviceRegistry := registry.NewServiceRegistry(
		env,
		pool,
		"",
		600,
		"",
	)
	serviceRegistry.Connect(redisHost)

	return &ServiceRuntime{
		shuttleHost:     shuttleHost,
		serviceRegistry: serviceRegistry,
	}

}

func (s *ServiceRuntime) ensureDockerClient() *docker.Client {
	if s.dockerClient == nil {
		endpoint := "unix:///var/run/docker.sock"
		client, err := docker.NewClient(endpoint)
		if err != nil {
			panic(err)
		}
		s.dockerClient = client

	}
	return s.dockerClient
}

func (s *ServiceRuntime) InspectImage(image string) (*docker.Image, error) {
	return s.ensureDockerClient().InspectImage(image)
}

func (s *ServiceRuntime) StopAllButLatest(stopCutoff int64) error {

	serviceConfigs, err := s.serviceRegistry.ListApps("")
	if err != nil {
		return err
	}

	containers, err := s.ensureDockerClient().ListContainers(docker.ListContainersOptions{
		All: false,
	})
	if err != nil {
		return err
	}

	for _, serviceConfig := range serviceConfigs {
		registry, repository, _ := utils.SplitDockerImage(serviceConfig.Version())
		latestName := serviceConfig.ContainerName()

		latestContainer, err := s.ensureDockerClient().InspectContainer(latestName)
		_, ok := err.(*docker.NoSuchContainer)
		// Expected container is not actually running. Skip it and leave old ones.
		if err != nil && ok {
			continue
		}

		for _, container := range containers {

			// We name all galaxy managed containers
			if len(container.Names) == 0 {
				continue
			}

			// Container name does match one that would be started w/ this service config
			if !serviceConfig.IsContainerVersion(strings.TrimPrefix(container.Names[0], "/")) {
				continue
			}

			containerReg, containerRepo, _ := utils.SplitDockerImage(container.Image)
			if containerReg == registry && containerRepo == repository && container.ID != latestContainer.ID &&
				container.Created < (time.Now().Unix()-stopCutoff) {

				// HACK: Docker 0.9 gets zombie containers randomly.  The only way to remove
				// them is to restart the docker daemon.  If we timeout once trying to stop
				// one of these containers, blacklist it and leave it running

				if _, ok := blacklistedContainerId[container.ID]; ok {
					log.Printf("Container %s blacklisted. Won't try to stop.\n", container.ID)
					continue
				}

				log.Printf("Stopping %s container %s\n", container.Image, container.ID[0:12])
				c := make(chan error, 1)
				go func() { c <- s.ensureDockerClient().StopContainer(container.ID, 10) }()
				select {
				case err := <-c:
					if err != nil {
						log.Printf("ERROR: Unable to stop container: %s\n", container.ID)
						continue
					}
				case <-time.After(20 * time.Second):
					blacklistedContainerId[container.ID] = true
					log.Printf("ERROR: Timed out trying to stop container. Zombie?. Blacklisting: %s\n", container.ID)
					continue
				}

				s.ensureDockerClient().RemoveContainer(docker.RemoveContainerOptions{
					ID:            container.ID,
					RemoveVolumes: true,
				})
			}
		}
	}

	return nil

}

func (s *ServiceRuntime) GetImageByName(img string) (*docker.APIImages, error) {
	imgs, err := s.ensureDockerClient().ListImages(true)
	if err != nil {
		panic(err)
	}

	for _, image := range imgs {
		if utils.StringInSlice(img, image.RepoTags) {
			return &image, nil
		}
	}
	return nil, nil

}

func (s *ServiceRuntime) RunCommand(serviceConfig *registry.ServiceConfig, cmd []string) (*docker.Container, error) {

	// see if we have the image locally
	_, err := s.PullImage(serviceConfig.Version())
	if err != nil {
		return nil, err
	}

	// setup env vars from etcd
	envVars := []string{
		"HOME=/",
		"PATH=" + "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOSTNAME=" + "app",
		"TERM=xterm",
	}

	for key, value := range serviceConfig.Env() {
		envVars = append(envVars, strings.ToUpper(key)+"="+value)
	}

	runCmd := []string{"/bin/bash", "-c", strings.Join(cmd, " ")}

	container, err := s.ensureDockerClient().CreateContainer(docker.CreateContainerOptions{
		Config: &docker.Config{
			Image:        serviceConfig.Version(),
			Env:          envVars,
			AttachStdout: true,
			AttachStderr: true,
			Cmd:          runCmd,
			OpenStdin:    false,
		},
	})

	if err != nil {
		return nil, err
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)
	go func(s *ServiceRuntime, containerId string) {
		<-c
		log.Println("Stopping container...")
		err := s.ensureDockerClient().StopContainer(containerId, 3)
		if err != nil {
			log.Printf("ERROR: Unable to stop container: %s", err)
		}
		err = s.ensureDockerClient().RemoveContainer(docker.RemoveContainerOptions{
			ID: containerId,
		})
		if err != nil {
			log.Printf("ERROR: Unable to stop container: %s", err)
		}

	}(s, container.ID)

	defer s.ensureDockerClient().RemoveContainer(docker.RemoveContainerOptions{
		ID: container.ID,
	})
	err = s.ensureDockerClient().StartContainer(container.ID,
		&docker.HostConfig{})

	if err != nil {
		return container, err
	}

	// FIXME: Hack to work around the race of attaching to a container before it's
	// actually running.  Tried polling the container and then attaching but the
	// output gets lost sometimes if the command executes very quickly. Not sure
	// what's going on.
	time.Sleep(1 * time.Second)

	err = s.ensureDockerClient().AttachToContainer(docker.AttachToContainerOptions{
		Container:    container.ID,
		OutputStream: os.Stdout,
		ErrorStream:  os.Stderr,
		Logs:         true,
		Stream:       false,
		Stdout:       true,
		Stderr:       true,
	})

	if err != nil {
		log.Printf("ERROR: Unable to attach to running container: %s", err.Error())
	}

	s.ensureDockerClient().WaitContainer(container.ID)

	return container, err
}

func (s *ServiceRuntime) StartInteractive(serviceConfig *registry.ServiceConfig) error {

	// see if we have the image locally
	_, err := s.PullImage(serviceConfig.Version())
	if err != nil {
		return err
	}

	args := []string{
		"run", "-rm", "-i",
	}
	for key, value := range serviceConfig.Env() {
		args = append(args, "-e")
		args = append(args, strings.ToUpper(key)+"="+value)
	}

	serviceConfigs, err := s.serviceRegistry.ListApps("")
	if err != nil {
		return err
	}

	for _, config := range serviceConfigs {
		for port, _ := range config.Ports() {
			args = append(args, "-e")
			args = append(args, strings.ToUpper(config.Name)+"_ADDR_"+port+"="+s.shuttleHost+":"+port)
		}
	}

	args = append(args, []string{"-t", serviceConfig.Version(), "/bin/bash"}...)
	// shell out to docker run to get signal forwarded and terminal setup correctly
	//cmd := exec.Command("docker", "run", "-rm", "-i", "-t", serviceConfig.Version(), "/bin/bash")
	cmd := exec.Command("docker", args...)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		log.Fatal(err)
	}

	err = cmd.Wait()
	if err != nil {
		fmt.Printf("Command finished with error: %v\n", err)
	}

	return err
}

func (s *ServiceRuntime) Start(serviceConfig *registry.ServiceConfig) (*docker.Container, error) {
	img := serviceConfig.Version()
	// see if we have the image locally
	_, err := s.PullImage(img)
	if err != nil {
		return nil, err
	}

	// setup env vars from etcd
	var envVars []string
	for key, value := range serviceConfig.Env() {
		envVars = append(envVars, strings.ToUpper(key)+"="+value)
	}

	serviceConfigs, err := s.serviceRegistry.ListApps("")
	if err != nil {
		return nil, err
	}

	for _, config := range serviceConfigs {
		for port, _ := range config.Ports() {
			// FIXME: Need a deterministic way to map local shuttle ports to remote services
			envVars = append(envVars, strings.ToUpper(config.Name)+"_ADDR_"+port+"="+s.shuttleHost+":"+port)
		}
	}

	containerName := serviceConfig.ContainerName()
	container, err := s.ensureDockerClient().InspectContainer(containerName)
	_, ok := err.(*docker.NoSuchContainer)
	if err != nil && !ok {
		return nil, err
	}

	if container == nil {
		container, err = s.ensureDockerClient().CreateContainer(docker.CreateContainerOptions{
			Name: containerName,
			Config: &docker.Config{
				Image: img,
				Env:   envVars,
			},
		})
		if err != nil {
			return nil, err
		}
	}

	err = s.ensureDockerClient().StartContainer(container.ID,
		&docker.HostConfig{
			PublishAllPorts: true,
		})

	if err != nil {
		return container, err
	}

	startedContainer, err := s.ensureDockerClient().InspectContainer(container.ID)
	for i := 0; i < 5; i++ {

		startedContainer, err = s.ensureDockerClient().InspectContainer(container.ID)
		if !startedContainer.State.Running {
			return nil, errors.New("Container stopped unexpectedly")
		}
		time.Sleep(1 * time.Second)
	}
	return startedContainer, err

}

func (s *ServiceRuntime) StartIfNotRunning(serviceConfig *registry.ServiceConfig) (bool, *docker.Container, error) {
	container, err := s.ensureDockerClient().InspectContainer(serviceConfig.ContainerName())
	_, ok := err.(*docker.NoSuchContainer)
	// Expected container is not actually running. Skip it and leave old ones.
	if (err != nil && ok) || container == nil {
		container, err := s.Start(serviceConfig)
		return true, container, err
	}

	if err != nil {
		return false, nil, err
	}

	containerName := strings.TrimPrefix(container.Name, "/")
	// check if container is the right version
	if !serviceConfig.IsContainerVersion(containerName) && serviceConfig.ContainerName() == container.Name {
		return false, container, nil
	}

	if containerName != serviceConfig.ContainerName() {
		container, err := s.Start(serviceConfig)
		return true, container, err
	}

	return false, container, nil

}

func (s *ServiceRuntime) PullImage(version string) (*docker.Image, error) {
	image, err := s.ensureDockerClient().InspectImage(version)
	if err != nil {
		return nil, err
	}

	if image != nil {
		return image, nil
	}

	registry, repository, _ := utils.SplitDockerImage(version)
	// No, pull it down locally
	pullOpts := docker.PullImageOptions{
		Repository:   repository,
		OutputStream: os.Stdout}

	dockerAuth := docker.AuthConfiguration{}
	if registry != "" && s.authConfig == nil {

		pullOpts.Repository = registry + "/" + repository
		pullOpts.Registry = registry

		currentUser, err := user.Current()
		if err != nil {
			panic(err)
		}

		// use ~/.dockercfg
		authConfig, err := auth.LoadConfig(currentUser.HomeDir)
		if err != nil {
			panic(err)
		}

		pullOpts.Registry = registry
		authCreds := authConfig.ResolveAuthConfig(registry)

		dockerAuth.Username = authCreds.Username
		dockerAuth.Password = authCreds.Password
		dockerAuth.Email = authCreds.Email
	}

	err = s.ensureDockerClient().PullImage(pullOpts, dockerAuth)
	if err != nil {
		return nil, err
	}
	return s.ensureDockerClient().InspectImage(version)

}
