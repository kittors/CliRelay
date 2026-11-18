package main

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

func main() {
	yamlData := `
port: 8317
redis:
  enable: true
  addr: "127.0.0.1:6379"
  password: "hello"
  db: 5
`
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Printf("Redis Enable: %v\n", cfg.Redis.Enable)
	fmt.Printf("Redis Addr: %v\n", cfg.Redis.Addr)
	fmt.Printf("Redis Password: %v\n", cfg.Redis.Password)
	fmt.Printf("Redis DB: %v\n", cfg.Redis.DB)
}
