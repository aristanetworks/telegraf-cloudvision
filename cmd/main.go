package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/arista-netdevops-community/Telegraf-Cloudvision-Telemetry/plugins/inputs/arista_cloudvision_telemtry"
	"github.com/influxdata/telegraf/plugins/common/shim"
)

var err error
var configFile = flag.String("config", "", "path to the config file for this plugin")
var pollInterval = flag.Duration("poll_interval", 1*time.Minute, "how often to send metrics")

func main() {
	flag.Parse()

	shim := shim.New()

	err = shim.LoadConfig(configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Err loading input: %s\n", err)
		os.Exit(1)
	}

	if err := shim.Run(*pollInterval); err != nil {
		fmt.Fprintf(os.Stderr, "Err: %s\n", err)
		os.Exit(1)
	}

}