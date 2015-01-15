package commander

import (
	"strconv"
	"strings"

	"github.com/litl/galaxy/config"
	"github.com/litl/galaxy/log"
	"github.com/litl/galaxy/utils"
	"github.com/ryanuber/columnize"
)

type RuntimeOptions struct {
	Ps          int
	Memory      string
	CPUShares   string
	VirtualHost string
	Port        string
}

func RuntimeList(configStore *config.Store, app, env, pool string) error {

	envs := []string{env}

	if env == "" {
		var err error
		envs, err = configStore.ListEnvs()
		if err != nil {
			return err
		}
	}

	columns := []string{"ENV | NAME | POOL | PS | MEM | VHOSTS | PORT"}

	for _, env := range envs {

		appList, err := configStore.ListApps(env)
		if err != nil {
			return err
		}

		for _, appCfg := range appList {

			if app != "" && appCfg.Name != app {
				continue
			}

			for _, p := range appCfg.RuntimePools() {

				if pool != "" && p != pool {
					continue
				}

				name := appCfg.Name
				ps := appCfg.GetProcesses(p)
				mem := appCfg.GetMemory(p)

				columns = append(columns, strings.Join([]string{
					env,
					name,
					p,
					strconv.FormatInt(int64(ps), 10),
					mem,
					appCfg.Env()["VIRTUAL_HOST"],
					appCfg.Env()["GALAXY_PORT"],
				}, " | "))
			}
		}
	}
	output, _ := columnize.SimpleFormat(columns)
	log.Println(output)
	return nil

}

func RuntimeSet(configStore *config.Store, app, env, pool string, options RuntimeOptions) (bool, error) {

	cfg, err := configStore.GetApp(app, env)
	if err != nil {
		return false, err
	}

	if options.Ps != 0 && options.Ps != cfg.GetProcesses(pool) {
		cfg.SetProcesses(pool, options.Ps)
	}

	if options.Memory != "" && options.Memory != cfg.GetMemory(pool) {
		cfg.SetMemory(pool, options.Memory)
	}

	vhosts := strings.Split(cfg.Env()["VIRTUAL_HOST"], ",")
	if options.VirtualHost != "" && !utils.StringInSlice(options.VirtualHost, vhosts) {
		vhosts = append(vhosts, options.VirtualHost)
		cfg.EnvSet("VIRTUAL_HOST", strings.Join(vhosts, ","))
	}

	if options.Port != "" {
		cfg.EnvSet("GALAXY_PORT", options.Port)
	}

	return configStore.UpdateApp(cfg, env)
}
