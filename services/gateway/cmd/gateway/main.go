package main

import (
	"fmt"

	"github.com/luksluki/eprocurement/internal/platform/config"
)

func main() {
	cfg := config.Load("gateway")
	fmt.Printf("gateway skeleton (env=%s, port=%s) — behaviour arrives in later issues\n", cfg.Env, cfg.Port)
}
