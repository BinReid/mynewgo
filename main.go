package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	port := flag.Int("port", 8080, "local HTTP port")
	database := flag.String("db", "data/monitor.db", "SQLite database path")
	parallel := flag.Int("parallel", 8, "maximum concurrent checks")
	timeout := flag.Duration("timeout", 5*time.Second, "timeout per HTTP check")
	flag.Parse()
	if *port < 1 || *port > 65535 || *parallel < 1 || *timeout <= 0 {
		return errors.New("port, parallel and timeout must be valid positive values")
	}
	store, err := openStore(*database)
	if err != nil {
		return err
	}
	defer store.db.Close()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	client := &http.Client{}
	defer client.CloseIdleConnections()
	monitor := Monitor{store: store, client: client, timeout: *timeout, parallel: *parallel}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		monitor.run(ctx)
	}()
	server := &http.Server{Handler: newWeb(store, *port), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	fmt.Printf("Uptime Monitor: http://%s\n", address)
	select {
	case err := <-serverDone:
		stop()
		<-monitorDone
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		<-monitorDone
		return err
	}
}
