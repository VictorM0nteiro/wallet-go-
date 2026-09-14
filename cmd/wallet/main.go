package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Println("wallet: iniciado")

	<-ctx.Done()

	log.Println("wallet: sinal recebido, encerrando")
}