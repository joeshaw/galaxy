package main

import (
	"fmt"
	"github.com/codegangsta/cli"
	"github.com/fsouza/go-dockerclient"
	"github.com/litl/galaxy/registry"
	"github.com/litl/galaxy/utils"
	"github.com/ryanuber/columnize"
	"os"
	"strings"
	"time"
)

func register(c *cli.Context) {

	initOrDie(c)

	for {

		containers, err := client.ListContainers(docker.ListContainersOptions{
			All: false,
		})
		if err != nil {
			panic(err)
		}

		outputBuffer.Log(strings.Join([]string{
			"CONTAINER ID", "REGISTRATION", "IMAGE",
			"EXTERNAL", "INTERNAL", "CREATED", "EXPIRES",
		}, " | "))

		for _, container := range containers {
			dockerContainer, err := client.InspectContainer(container.ID)
			if err != nil {
				fmt.Printf("ERROR: Unable to inspect container %s: %s. Skipping.\n", container.ID, err)
				continue
			}

			_, repository, tag := utils.SplitDockerImage(dockerContainer.Config.Image)

			env := make(map[string]string)
			for _, entry := range dockerContainer.Config.Env {
				firstSeparator := strings.Index(entry, "=")
				key := entry[0:firstSeparator]
				value := entry[firstSeparator+1:]
				env[key] = value
			}

			serviceConfig := &registry.ServiceConfig{
				Name:    repository,
				Env:     env,
				Version: tag,
			}

			existingConfig, err := serviceRegistry.GetServiceConfig(repository)
			if err != nil {
				fmt.Printf("ERROR: Unable to determine if app %s exists: %s. Skipping.\n", repository, err)
				continue
			}
			if existingConfig == nil {
				// container isn't a galaxy app. skip it.
				continue
			}

			err = serviceRegistry.RegisterService(dockerContainer, serviceConfig)
			if err != nil {
				fmt.Printf("ERROR: Could not register %s: %s\n",
					serviceConfig.Name, err)
				os.Exit(1)
			}

		}

		if !c.Bool("loop") {
			break
		}
		time.Sleep(10 * time.Second)

	}

	result, _ := columnize.SimpleFormat(outputBuffer.Output)
	fmt.Println(result)

}