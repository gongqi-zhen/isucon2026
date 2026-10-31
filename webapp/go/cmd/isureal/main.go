package main

import "example.com/isureal-company/isureal"

// 2026-09-23

func main() {
	config := isureal.ConfigFromEnv()
	server := isureal.NewServer(config)
	if err := server.Start(config.ListenAddr); err != nil {
		panic("failed to start server: " + err.Error())
	}
}
