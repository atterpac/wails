package commands

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/wailsapp/wails/v3/internal/flags"
)

const defaultVitePort = 9245
const wailsVitePort = "WAILS_VITE_PORT"

type DevOptions struct {
	flags.Common

	Config   string `description:"The config file including path" default:"./build/config.yml"`
	VitePort int    `name:"port" description:"Specify the vite dev server port"`
	Secure   bool   `name:"s" description:"Enable HTTPS"`
	Pipeline bool   `name:"pipeline" description:"Use the typed Wails development pipeline"`
	Plan     bool   `name:"plan" description:"Print the resolved development plan without executing it"`
	JSON     bool   `name:"json" description:"Print the development plan as JSON (requires --plan)"`
}

func Dev(options *DevOptions) error {
	if options.JSON && !options.Plan {
		return fmt.Errorf("--json requires --plan")
	}
	host := "localhost"

	// flag takes precedence over environment variable
	var port int
	if options.VitePort != 0 {
		port = options.VitePort
	} else if p, err := strconv.Atoi(os.Getenv(wailsVitePort)); err == nil {
		port = p
	} else {
		port = defaultVitePort
	}

	// Inspection is side-effect free; only execution probes whether the port is
	// available before starting the frontend process.
	if !options.Plan {
		l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
		if err != nil {
			return err
		}
		if err = l.Close(); err != nil {
			return err
		}
	}

	// Set environment variable for the dev:frontend task
	os.Setenv(wailsVitePort, strconv.Itoa(port))

	// Set url of frontend dev server
	if options.Secure {
		os.Setenv("FRONTEND_DEVSERVER_URL", fmt.Sprintf("https://%s:%d", host, port))
	} else {
		os.Setenv("FRONTEND_DEVSERVER_URL", fmt.Sprintf("http://%s:%d", host, port))
	}

	// Environment variables such as WAILS_MCP imply extra build tags. Export them
	// via EXTRA_TAGS so the project Taskfile includes them in dev builds.
	if tags := envTags(); len(tags) > 0 {
		os.Setenv("EXTRA_TAGS", mergeTags(os.Getenv("EXTRA_TAGS"), tags...))
	}

	if options.Pipeline || options.Plan {
		plan, err := resolveTypedDevPlan(options, host, port)
		if err != nil {
			return err
		}
		if options.Plan {
			DisableFooter = true
			if options.JSON {
				encoder := json.NewEncoder(os.Stdout)
				encoder.SetIndent("", "  ")
				return encoder.Encode(plan)
			}
			return writeDevPlan(os.Stdout, plan)
		}
		return executeTypedDevPlan(options, plan)
	}

	return Watcher(&WatcherOptions{
		Config: options.Config,
	})
}
