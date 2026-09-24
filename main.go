// Command micseq-bot is a Mumble bot that runs YY-style 麦序 (speaking
// queues) in channels whose names carry a marker such as
// "会议室 [麦序/发言时间: 300秒/下麦转12]".
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	bot := NewBot(cfg)
	if err := bot.LoadState(); err != nil {
		log.Printf("ignoring saved state: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bot.Run(ctx)
	log.Print("stopped")
}
