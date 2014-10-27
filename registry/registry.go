package registry

import (
	"errors"
	"fmt"
	"path"
	"strings"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/litl/galaxy/log"
	"github.com/litl/galaxy/utils"
)

/*
All config opbects in redis will be stored in a hash with an id key.
Services will have id, version and environment keys; while Hosts will have id
and location keys.

TODO: IMPORTANT: make an atomic compare-and-swap script to save configs, or
      switch to ORDERED SETS and log changes
*/

const (
	DefaultTTL = 60
)

type ServiceRegistry struct {
	backend      RegistryBackend
	HostIP       string
	Hostname     string
	TTL          uint64
	HostSSHAddr  string
	OutputBuffer *utils.OutputBuffer
	pollCh       chan bool
	redisHost    string
}

func NewServiceRegistry(hostIp string, ttl uint64, sshAddr string) *ServiceRegistry {
	return &ServiceRegistry{
		HostIP:      hostIp,
		TTL:         ttl,
		HostSSHAddr: sshAddr,
		pollCh:      make(chan bool),
	}

}

// Build the Redis Pool
func (r *ServiceRegistry) Connect(redisHost string) {

	r.redisHost = redisHost
	r.backend = &RedisBackend{
		RedisHost: redisHost,
	}
	r.backend.Connect()
}

func (r *ServiceRegistry) newServiceRegistration(container *docker.Container) *ServiceRegistration {
	//FIXME: We're using the first found port and assuming it's tcp.
	//How should we handle a service that exposes multiple ports
	//as well as tcp vs udp ports.
	var externalPort, internalPort string
	for k, v := range container.NetworkSettings.Ports {
		if len(v) > 0 {
			externalPort = v[0].HostPort
			internalPort = k.Port()
			break
		}
	}

	serviceRegistration := ServiceRegistration{
		ContainerName: container.Name,
		ContainerID:   container.ID,
		StartedAt:     container.Created,
		Image:         container.Config.Image,
	}

	if externalPort != "" && internalPort != "" {
		serviceRegistration.ExternalIP = r.HostIP
		serviceRegistration.InternalIP = container.NetworkSettings.IPAddress
		serviceRegistration.ExternalPort = externalPort
		serviceRegistration.InternalPort = internalPort
	}
	return &serviceRegistration
}

// TODO: log or return error?
func (r *ServiceRegistry) CountInstances(app, env string) int {
	// TODO: convert to SCAN
	// TODO: Should this just sum hosts? (this counts all services on all hosts)
	matches, err := r.backend.Keys(path.Join(env, "*", "hosts", "*", app))
	if err != nil {
		log.Printf("ERROR: could not count instances - %s\n", err)
	}

	return len(matches)
}

func (r *ServiceRegistry) PoolExists(env, pool string) (bool, error) {
	pools, err := r.ListPools(env)
	if err != nil {
		return false, err
	}
	_, ok := pools[pool]
	return ok, nil
}

func (r *ServiceRegistry) AppExists(app, env string) (bool, error) {
	matches, err := r.backend.Keys(path.Join(env, app, "*"))
	if err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

func (r *ServiceRegistry) ListAssignments(env, pool string) ([]string, error) {
	return r.backend.Members(path.Join(env, "pools", pool))
}

func (r *ServiceRegistry) AssignApp(app, env, pool string) (bool, error) {
	if exists, err := r.AppExists(app, env); !exists || err != nil {
		return false, err
	}

	if exists, err := r.PoolExists(env, pool); !exists || err != nil {
		return false, errors.New(fmt.Sprintf("pool %s does not exist", pool))
	}

	added, err := r.backend.AddMember(path.Join(env, "pools", pool), app)
	if err != nil {
		return false, err
	}

	err = r.NotifyRestart(app, env)
	if err != nil {
		return added == 1, err
	}

	return added == 1, nil
}

func (r *ServiceRegistry) UnassignApp(app, env, pool string) (bool, error) {
	//FIXME: Scan keys to make sure there are no deploye apps before
	//deleting the pool.

	//FIXME: Shutdown the associated auto-scaling groups tied to the
	//pool

	removed, err := r.backend.RemoveMember(path.Join(env, "pools", pool), app)
	if removed == 0 || err != nil {
		return false, err
	}

	err = r.NotifyRestart(app, env)
	if err != nil {
		return removed == 1, err
	}

	return removed == 1, nil
}

func (r *ServiceRegistry) CreatePool(name, env string) (bool, error) {
	//FIXME: Create an associated auto-scaling groups tied to the
	//pool

	added, err := r.backend.AddMember(path.Join(env, "pools", "*"), name)
	if err != nil {
		return false, err
	}
	return added == 1, nil
}

func (r *ServiceRegistry) DeletePool(pool, env string) (bool, error) {
	//FIXME: Scan keys to make sure there are no deploye apps before
	//deleting the pool.

	//FIXME: Shutdown the associated auto-scaling groups tied to the
	//pool

	assignments, err := r.ListAssignments(env, pool)
	if err != nil {
		return false, err
	}

	if len(assignments) > 0 {
		return false, nil
	}

	removed, err := r.backend.RemoveMember(path.Join(env, "pools", "*"), pool)
	if err != nil {
		return false, err
	}
	return removed == 1, nil
}

func (r *ServiceRegistry) ListPools(env string) (map[string][]string, error) {
	assignments := make(map[string][]string)

	matches, err := r.backend.Members(path.Join(env, "pools", "*"))
	if err != nil {
		return assignments, err
	}

	for _, pool := range matches {

		members, err := r.ListAssignments(env, pool)
		if err != nil {
			return assignments, err
		}
		assignments[pool] = members
	}

	return assignments, nil
}

func (r *ServiceRegistry) CreateApp(app, env string) (bool, error) {
	if exists, err := r.AppExists(app, env); exists || err != nil {
		return false, err
	}

	emptyConfig := NewServiceConfig(app, "")
	emptyConfig.environmentVMap.Set("ENV", env)

	return r.SetServiceConfig(emptyConfig, env)
}

func (r *ServiceRegistry) DeleteApp(app, env string) (bool, error) {

	pools, err := r.ListPools(env)
	if err != nil {
		return false, err
	}

	for pool, assignments := range pools {
		if utils.StringInSlice(app, assignments) {
			return false, errors.New(fmt.Sprintf("app is assigned to pool %s", pool))
		}
	}

	svcCfg, err := r.GetServiceConfig(app, env)
	if err != nil {
		return false, err
	}

	if svcCfg == nil {
		return true, nil
	}

	return r.DeleteServiceConfig(svcCfg, env)
}

func (r *ServiceRegistry) ListApps(env string) ([]ServiceConfig, error) {
	// TODO: convert to scan
	apps, err := r.backend.Keys(path.Join(env, "*", "environment"))
	if err != nil {
		return nil, err
	}

	// TODO: is it OK to error out early?
	var appList []ServiceConfig
	for _, app := range apps {
		parts := strings.Split(app, "/")

		// app entries should be 3 parts, /env/pool/app
		if len(parts) != 3 {
			continue
		}

		// we don't want host keys
		if parts[1] == "hosts" {
			continue
		}

		cfg, err := r.GetServiceConfig(parts[1], env)
		if err != nil {
			return nil, err
		}

		appList = append(appList, *cfg)
	}

	return appList, nil
}

func (r *ServiceRegistry) ListEnvs() ([]string, error) {
	envs := []string{}
	apps, err := r.backend.Keys(path.Join("*", "*", "environment"))
	if err != nil {
		return nil, err
	}

	for _, app := range apps {
		parts := strings.Split(app, "/")
		if !utils.StringInSlice(parts[0], envs) {
			envs = append(envs, parts[0])
		}
	}
	return envs, nil
}
