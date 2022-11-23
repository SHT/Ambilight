package config

import (
	"bytes"
	"encoding/json"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultMode string      `yaml:"defaultMode" json:"defaultMode"`
	CaptureType string      `yaml:"captureType" json:"captureType"`
	Server      Server      `yaml:"server" json:"server"`
	Displays    [][]Display `yaml:"displays" json:"displays"`
	Audio       AudioConfig `yaml:"audio" json:"audio"`
	Segments    []Segment   `yaml:"segments" json:"segments"`
}

type AudioConfig struct {
	Colors     []string `yaml:"colors" json:"colors"`
	WindowSize int      `yaml:"windowSize" json:"windowSize"`
}

type Server struct {
	Host       string `yaml:"host" json:"host"`
	Port       int    `yaml:"port" json:"port"`
	Leds       int    `yaml:"leds" json:"leds"`
	StripType  string `yaml:"stripType" json:"stripType"`
	GpioPin    int    `yaml:"gpioPin" json:"gpioPin"`
	Brightness int    `yaml:"brightness" json:"brightness"`
	BlackPoint int    `yaml:"blackPoint" json:"blackPoint"`
}

type Display struct {
	Segment   int    `yaml:"segment" json:"segment"`
	Width     int    `yaml:"width" json:"width"`
	Height    int    `yaml:"height" json:"height"`
	Left      int    `yaml:"left" json:"left"`
	Top       int    `yaml:"top" json:"top"`
	Bounds    Bounds `yaml:"bounds" json:"bounds"`
	Framerate int    `yaml:"framerate" json:"framerate"`
}

type Bounds struct {
	From Vector2 `yaml:"from" json:"from"`
	To   Vector2 `yaml:"to" json:"to"`
}

type Vector2 struct {
	X int `yaml:"x" json:"x"`
	Y int `yaml:"y" json:"y"`
}

type Segment struct {
	Id   int `yaml:"id" json:"id"`
	Leds int `yaml:"leds" json:"leds"`
}

func (c *Config) Save() error {
	var b []byte

	switch c.format {
	case "json":
		var err error
		b, err = json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
	case "yaml":
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		err := enc.Encode(c)
		if err != nil {
			return err
		}

		b = buf.Bytes()
	}

	err := os.WriteFile(c.name, b, 0644)
	if err != nil {
		return err
	}

	return nil
}

func Load() (*Config, error) {
	validCfgs := map[string]string{
		"ledctl.json": "json",
		"ledctl.yaml": "yaml",
		"ledctl.yml":  "yaml",
	}

	for name, format := range validCfgs {
		if _, err := os.Stat(name); err == nil {
			b, err := os.ReadFile(name)
			if err != nil {
				return nil, err
			}

			var c Config

			switch format {
			case "json":
				if err := json.Unmarshal(b, &c); err != nil {
					return nil, err
				}

				c.format = "json"
			case "yaml":
				if err := yaml.Unmarshal(b, &c); err != nil {
					return nil, err
				}

				c.format = "yaml"
			}

			c.name = name

			return &c, nil
		}
	}

	return createDefault()
}

func createDefault() (*Config, error) {
	c := Config{
		DefaultMode: "video",
		CaptureType: "bitblt",
		Server: Server{
			Host:       "0.0.0.0",
			Port:       4197,
			Leds:       100,
			StripType:  "grb",
			GpioPin:    18,
			Brightness: 255,
			BlackPoint: 0,
		},
		Segments: []Segment{
			{
				Id:   0,
				Leds: 100,
			},
		},
		Displays: [][]Display{
			{
				{
					Segment:   0,
					Width:     1920,
					Height:    1080,
					Left:      0,
					Top:       0,
					Framerate: 60,
					Bounds: Bounds{
						From: Vector2{X: 0, Y: 0},
						To:   Vector2{X: 0, Y: 0},
					},
				},
			},
		},
		Audio: AudioConfig{
			Colors: []string{
				"#355c7d",
				"#725a7c",
				"#c66c86",
				"#ff7582",
			},
			WindowSize: 80,
		},
	}

	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}

	err = os.WriteFile("ledctl.json", b, 0644)
	if err != nil {
		return nil, err
	}

	return &c, nil
}
