// Command quorum-server runs the quorum chat server.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
	"github.com/layer8/quorum/internal/auth"
	"github.com/layer8/quorum/internal/hub"
	"github.com/layer8/quorum/internal/server"
	"github.com/layer8/quorum/internal/store"
)

func main() {
	listen := flag.String("listen", ":8443", "listen address")
	certFile := flag.String("cert", "certs/server.pem", "TLS certificate")
	keyFile := flag.String("key", "certs/server-key.pem", "TLS private key")
	dbPath := flag.String("db", "quorum.db", "SQLite database path")
	initAdmin := flag.String("init-admin", "", "create an admin user with this name, then exit")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid log level %q\n", *logLevel)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	st, err := store.Open(*dbPath)
	if err != nil {
		fatal(logger, "open store", err)
	}
	defer st.Close()

	if *initAdmin != "" {
		if err := createAdmin(st, *initAdmin); err != nil {
			fatal(logger, "init admin", err)
		}
		return
	}

	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		fatal(logger, "load TLS keypair", err)
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})

	authn := auth.NewAuthenticator(st)
	h := hub.New()

	grpcServer := grpc.NewServer(
		grpc.Creds(creds),
		grpc.UnaryInterceptor(authn.UnaryInterceptor()),
		grpc.StreamInterceptor(authn.StreamInterceptor()),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 15 * time.Second, PermitWithoutStream: true}),
	)
	quorumv1.RegisterAuthServiceServer(grpcServer, server.NewAuthService(st))
	quorumv1.RegisterChatServiceServer(grpcServer, server.NewChatService(st, h, logger))
	quorumv1.RegisterAdminServiceServer(grpcServer, server.NewAdminService(st, h))

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		fatal(logger, "listen", err)
	}

	// Periodically clear expired sessions.
	go func() {
		for range time.Tick(time.Hour) {
			if err := st.DeleteExpiredSessions(context.Background()); err != nil {
				logger.Warn("session cleanup failed", "err", err)
			}
		}
	}()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		logger.Info("shutting down")
		grpcServer.GracefulStop()
	}()

	logger.Info("quorum server listening", "addr", *listen, "db", *dbPath)
	if err := grpcServer.Serve(lis); err != nil {
		fatal(logger, "serve", err)
	}
}

func createAdmin(st *store.Store, username string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("empty username")
	}
	password, err := readPassword(fmt.Sprintf("password for %s: ", username))
	if err != nil {
		return err
	}
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	phc, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	u := &store.User{ID: store.NewID(), Username: username, PasswordHash: phc, Role: "admin"}
	if err := st.CreateUser(context.Background(), u); err != nil {
		return err
	}
	fmt.Printf("created admin user %q (id %s)\n", username, u.ID)
	return nil
}

func readPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(b), err
	}
	// Non-interactive (piped) input for scripting/tests.
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func fatal(logger *slog.Logger, msg string, err error) {
	logger.Error(msg, "err", err)
	os.Exit(1)
}
