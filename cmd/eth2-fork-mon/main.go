package main

import (
	"flag"
	"log"
	"os"

	"github.com/ralexstokes/eth2-fork-mon/pkg/monitor"

	"gopkg.in/yaml.v2"
)

var configFile = flag.String("config-file", "/config.yaml", "path to configuration")
var outputDirectory = flag.String("output-dir", "public", "path to web assets")

func main() {
	flag.Parse()

	configFile, err := os.Open(*configFile)
	if err != nil {
		log.Fatal(err)
	}

	decoder := yaml.NewDecoder(configFile)
	config := &monitor.Config{}
	err = decoder.Decode(config)
	if err != nil {
		log.Fatal(err)
	}
	err = configFile.Close()
	if err != nil {
		log.Fatal(err)
	}

	config.OutputDir = *outputDirectory
	config.Eth2.SecondsPerSlot = 12
	config.Eth2.SlotsPerEpoch = 32
	forkMonitor := monitor.FromConfig(config)

	err = forkMonitor.Start()
	if err != nil {
		log.Fatal(err)
	}

	err = forkMonitor.Serve()
	if err != nil {
		log.Fatal(err)
	}
}
