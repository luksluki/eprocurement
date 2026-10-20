package main

import (
	"fmt"

	"github.com/luksluki/eprocurement/internal/platform/config"
)

func main() {
	cfg := config.Load("procurement")
	fmt.Printf("procurement skeleton (env=%s, port=%s) — behaviour arrives in later issues\n", cfg.Env, cfg.Port)
}
