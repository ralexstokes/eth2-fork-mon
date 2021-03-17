package monitor

type Eth2Config struct {
	SecondsPerSlot int    `json:"seconds_per_slot" yaml:"seconds_per_slot"`
	GenesisTime    int    `json:"genesis_time" yaml:"genesis_time"`
	SlotsPerEpoch  int    `json:"slots_per_epoch" yaml:"slots_per_epoch"`
	Network        string `json:"network" yaml:"network"`
}

type Config struct {
	Endpoints           []string
	Eth2                Eth2Config
	OutputDir           string
	EtherscanAPIKey     string `yaml:"etherscan_api_key"`
	MillisecondsTimeout int    `yaml:"http_timeout_milliseconds"`
	WSProviderEndpoint  string `yaml:"weak_subjectivity_provider_endpoint"`
}
