package registry

import (
	"log"
	"sync"
	"time"

	"github.com/garyburd/redigo/redis"
)

func (r *ServiceRegistry) CheckForChangesNow() {
	r.pollCh <- true
}

func (r *ServiceRegistry) checkForChanges(changes chan *ConfigChange) {
	lastVersion := make(map[string]int64)
	for {
		serviceConfigs, err := r.ListApps("")
		if err != nil {
			changes <- &ConfigChange{
				Error: err,
			}
			time.Sleep(5 * time.Second)
			continue
		}

		for _, config := range serviceConfigs {
			lastVersion[config.Name] = config.ID()
		}
		break

	}

	for {
		<-r.pollCh
		serviceConfigs, err := r.ListApps("")
		if err != nil {
			changes <- &ConfigChange{
				Error: err,
			}
			continue
		}
		for _, changedConfig := range serviceConfigs {
			changeCopy := changedConfig
			if changedConfig.ID() != lastVersion[changedConfig.Name] {
				lastVersion[changedConfig.Name] = changedConfig.ID()
				changes <- &ConfigChange{
					ServiceConfig: &changeCopy,
				}
			}
		}
	}
}

func (r *ServiceRegistry) checkForChangePeriodically(stop chan struct{}) {
	// TODO: default polling interval
	ticker := time.NewTicker(10 * time.Second)
	for {
		select {
		case <-stop:
			ticker.Stop()
			return
		case <-ticker.C:
			r.CheckForChangesNow()
		}
	}
}

func (r *ServiceRegistry) notifyChanged() error {
	conn := r.redisPool.Get()
	defer conn.Close()
	// TODO: received count ignored, use it somehow?
	_, err := redis.Int(conn.Do("PUBLISH", "galaxy", "config"))
	if err != nil {
		return err
	}
	return nil
}

func (r *ServiceRegistry) subscribeChanges() {
	var wg sync.WaitGroup

	redisPool := redis.Pool{
		MaxIdle:     1,
		IdleTimeout: 0,
		Dial: func() (redis.Conn, error) {
			c, err := redis.Dial("tcp", r.redisHost)
			if err != nil {
				return nil, err
			}
			return c, err
		},
		// test every connection for now
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			if err != nil {
				defer c.Close()
			}
			return err
		},
	}

	for {

		conn := redisPool.Get()
		defer conn.Close()
		if conn.Err() != nil {
			conn.Close()
			log.Printf("ERROR: %v\n", conn.Err())
			time.Sleep(5 * time.Second)
			r.reconnectRedis()
			continue
		}

		wg.Add(2)
		psc := redis.PubSubConn{Conn: conn}
		go func() {
			defer wg.Done()
			for {
				switch n := psc.Receive().(type) {
				case redis.Message:
					if string(n.Data) == "config" {
						log.Printf("Config changed. Re-deploying containers.\n")
						r.CheckForChangesNow()
					} else {
						log.Printf("Ignoring notification: %s %s\n", n.Channel, n.Data)
					}

				case error:
					psc.Close()
					log.Printf("ERROR: %v\n", n)
					return
				}
			}
		}()

		go func() {
			defer wg.Done()
			psc.Subscribe("galaxy")
			log.Printf("Monitoring for config changes on channel: galaxy\n")
		}()
		wg.Wait()
	}
}

func (r *ServiceRegistry) Watch(changes chan *ConfigChange, stop chan struct{}) {
	go r.checkForChanges(changes)
	go r.checkForChangePeriodically(stop)
	go r.subscribeChanges()
}