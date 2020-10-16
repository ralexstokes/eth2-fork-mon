package monitor

type Eth2Config struct {
	SecondsPerSlot int `json:"seconds_per_slot" yaml:"seconds_per_slot"`
	GenesisTime    int `json:"genesis_time" yaml:"genesis_time"`
}

type Config struct {
	Endpoints []string
	Eth2      Eth2Config
}
