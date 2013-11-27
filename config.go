package proxy

import (
	"encoding/json"
	"os"
)

type (
	SSL struct {
		Certificate string `json:"certificate"`
		Key         string `json:"key"`
	}
	Entry struct {
		Endpoints []string `json:"endpoints"`
		SSL       SSL      `json:"ssl"`
	}
	Config struct {
		Routes  map[string]Entry `json:"routes"`
		Port    int              `json:"port"`
		SSLPort int              `json:"ssl_port"`
	}
)

func GetConfig(filename string) (*Config, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	err = json.NewDecoder(f).Decode(&cfg)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (this *Config) Save(filename string) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	return json.NewEncoder(f).Encode(this)
}
