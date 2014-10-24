package registry

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	docker "github.com/fsouza/go-dockerclient"
)

type ServiceRegistration struct {
	Name          string            `json:"NAME,omitempty"`
	ExternalIP    string            `json:"EXTERNAL_IP,omitempty"`
	ExternalPort  string            `json:"EXTERNAL_PORT,omitempty"`
	InternalIP    string            `json:"INTERNAL_IP,omitempty"`
	InternalPort  string            `json:"INTERNAL_PORT,omitempty"`
	ContainerID   string            `json:"CONTAINER_ID"`
	ContainerName string            `json:"CONTAINER_NAME"`
	Image         string            `json:"IMAGE,omitempty"`
	ImageId       string            `json:"IMAGE_ID,omitempty"`
	StartedAt     time.Time         `json:"STARTED_AT"`
	Expires       time.Time         `json:"-"`
	Path          string            `json:"-"`
	VirtualHosts  []string          `json:"VIRTUAL_HOSTS"`
	Port          string            `json:"PORT"`
	ErrorPages    map[string]string `json:"ERROR_PAGES,omitempty"`
}

func (s *ServiceRegistration) Equals(other ServiceRegistration) bool {
	return s.ExternalIP == other.ExternalIP &&
		s.ExternalPort == other.ExternalPort &&
		s.InternalIP == other.InternalIP &&
		s.InternalPort == other.InternalPort
}

func (s *ServiceRegistration) addr(ip, port string) string {
	if ip != "" && port != "" {
		return fmt.Sprint(ip, ":", port)
	}
	return ""

}
func (s *ServiceRegistration) ExternalAddr() string {
	return s.addr(s.ExternalIP, s.ExternalPort)
}

func (s *ServiceRegistration) InternalAddr() string {
	return s.addr(s.InternalIP, s.InternalPort)
}

func (r *ServiceRegistry) RegisterService(env string, container *docker.Container, serviceConfig *ServiceConfig) (*ServiceRegistration, error) {
	registrationPath := path.Join(env, r.Pool, "hosts", r.HostIP, serviceConfig.Name)

	serviceRegistration := r.newServiceRegistration(container)
	serviceRegistration.Name = serviceConfig.Name
	serviceRegistration.ImageId = serviceConfig.VersionID()

	environment := serviceConfig.Env()

	vhosts := environment["VIRTUAL_HOST"]
	serviceRegistration.VirtualHosts = strings.Split(vhosts, ",")

	errorPages := make(map[string]string)

	// scan environment variables for the VIRTUAL_HOST_%d pattern
	// but save the original variable and url.
	for vhostCode, url := range environment {
		code := 0
		n, err := fmt.Sscanf(vhostCode, "VIRTUAL_HOST_%d", &code)
		if err != nil || n == 0 {
			continue
		}

		errorPages[vhostCode] = url
	}

	if len(errorPages) > 0 {
		serviceRegistration.ErrorPages = errorPages
	}

	serviceRegistration.VirtualHosts = strings.Split(vhosts, ",")

	serviceRegistration.Port = serviceConfig.Env()["GALAXY_PORT"]

	jsonReg, err := json.Marshal(serviceRegistration)
	if err != nil {
		return nil, err
	}

	// TODO: use a compare-and-swap SCRIPT
	_, err = r.backend.Set(registrationPath, "location", string(jsonReg))
	if err != nil {
		return nil, err
	}

	_, err = r.backend.Expire(registrationPath, r.TTL)
	if err != nil {
		return nil, err
	}
	serviceRegistration.Expires = time.Now().UTC().Add(time.Duration(r.TTL) * time.Second)

	return serviceRegistration, nil
}

func (r *ServiceRegistry) UnRegisterService(env string, container *docker.Container, serviceConfig *ServiceConfig) (*ServiceRegistration, error) {

	registrationPath := path.Join(env, r.Pool, "hosts", r.HostIP, serviceConfig.Name)

	registration, err := r.GetServiceRegistration(env, container, serviceConfig)
	if err != nil {
		return registration, err
	}

	_, err = r.backend.Delete(registrationPath)
	if err != nil {
		return registration, err
	}

	return registration, nil
}

func (r *ServiceRegistry) GetServiceRegistration(env string, container *docker.Container, serviceConfig *ServiceConfig) (*ServiceRegistration, error) {

	regPath := path.Join(env, r.Pool, "hosts", r.HostIP, serviceConfig.Name)

	existingRegistration := ServiceRegistration{
		Path: regPath,
	}

	location, err := r.backend.Get(regPath, "location")

	if err != nil {
		return nil, err
	}

	if location != "" {
		err = json.Unmarshal([]byte(location), &existingRegistration)
		if err != nil {
			return nil, err
		}

		expires, err := r.backend.Ttl(regPath)
		if err != nil {
			return nil, err
		}
		existingRegistration.Expires = time.Now().UTC().Add(time.Duration(expires) * time.Second)
		return &existingRegistration, nil
	}

	return nil, nil
}

func (r *ServiceRegistry) IsRegistered(env string, container *docker.Container, serviceConfig *ServiceConfig) (bool, error) {

	reg, err := r.GetServiceRegistration(env, container, serviceConfig)
	return reg != nil, err
}

// TODO: get all ServiceRegistrations
func (r *ServiceRegistry) ListRegistrations(env string) ([]ServiceRegistration, error) {

	// TODO: convert to scan
	keys, err := r.backend.Keys(path.Join(env, "*", "hosts", "*", "*"))
	if err != nil {
		return nil, err
	}

	var regList []ServiceRegistration
	for _, key := range keys {

		val, err := r.backend.Get(key, "location")
		if err != nil {
			return nil, err
		}

		svcReg := ServiceRegistration{
			Name: path.Base(key),
		}
		err = json.Unmarshal([]byte(val), &svcReg)
		if err != nil {
			return nil, err
		}

		svcReg.Path = key

		regList = append(regList, svcReg)
	}

	return regList, nil
}
