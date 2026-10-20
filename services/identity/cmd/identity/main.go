package main

import (
	"fmt"

	"github.com/luksluki/eprocurement/internal/platform/config"
)

func main() {
	cfg := config.Load("identity")
	fmt.Printf("identity skeleton (env=%s, port=%s) — behaviour arrives in later issues\n", cfg.Env, cfg.Port)
}
