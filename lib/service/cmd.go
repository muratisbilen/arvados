// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0

// package service provides a cmd.Handler that brings up a system service.
package service

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"git.curoverse.com/arvados.git/lib/cmd"
	"git.curoverse.com/arvados.git/lib/config"
	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"git.curoverse.com/arvados.git/sdk/go/ctxlog"
	"git.curoverse.com/arvados.git/sdk/go/httpserver"
	"github.com/coreos/go-systemd/daemon"
	"github.com/sirupsen/logrus"
)

type Handler interface {
	http.Handler
	CheckHealth() error
}

type NewHandlerFunc func(_ context.Context, _ *arvados.Cluster, _ *arvados.NodeProfile, token string) Handler

type command struct {
	newHandler NewHandlerFunc
	svcName    arvados.ServiceName
	ctx        context.Context // enables tests to shutdown service; no public API yet
}

// Command returns a cmd.Handler that loads site config, calls
// newHandler with the current cluster and node configs, and brings up
// an http server with the returned handler.
//
// The handler is wrapped with server middleware (adding X-Request-ID
// headers, logging requests/responses, etc).
func Command(svcName arvados.ServiceName, newHandler NewHandlerFunc) cmd.Handler {
	return &command{
		newHandler: newHandler,
		svcName:    svcName,
		ctx:        context.Background(),
	}
}

func (c *command) RunCommand(prog string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	log := ctxlog.New(stderr, "json", "info")

	var err error
	defer func() {
		if err != nil {
			log.WithError(err).Info("exiting")
		}
	}()
	flags := flag.NewFlagSet("", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFile := flags.String("config", arvados.DefaultConfigFile, "Site configuration `file`")
	nodeProfile := flags.String("node-profile", "", "`Name` of NodeProfiles config entry to use (if blank, use $ARVADOS_NODE_PROFILE or hostname reported by OS)")
	err = flags.Parse(args)
	if err == flag.ErrHelp {
		err = nil
		return 0
	} else if err != nil {
		return 2
	}
	cfg, err := config.LoadFile(*configFile, log)
	if err != nil {
		return 1
	}
	cluster, err := cfg.GetCluster("")
	if err != nil {
		return 1
	}
	log = ctxlog.New(stderr, cluster.SystemLogs.Format, cluster.SystemLogs.LogLevel).WithFields(logrus.Fields{
		"PID": os.Getpid(),
	})
	ctx := ctxlog.Context(c.ctx, log)

	profileName := *nodeProfile
	if profileName == "" {
		profileName = os.Getenv("ARVADOS_NODE_PROFILE")
	}
	profile, err := cluster.GetNodeProfile(profileName)
	if err != nil {
		return 1
	}
	listen := profile.ServicePorts()[c.svcName]
	if listen == "" {
		err = fmt.Errorf("configuration does not enable the %s service on this host", c.svcName)
		return 1
	}

	if cluster.SystemRootToken == "" {
		log.Warn("SystemRootToken missing from cluster config, falling back to ARVADOS_API_TOKEN environment variable")
		cluster.SystemRootToken = os.Getenv("ARVADOS_API_TOKEN")
	}
	if cluster.Services.Controller.ExternalURL.Host == "" {
		log.Warn("Services.Controller.ExternalURL missing from cluster config, falling back to ARVADOS_API_HOST(_INSECURE) environment variables")
		u, err := url.Parse("https://" + os.Getenv("ARVADOS_API_HOST"))
		if err != nil {
			err = fmt.Errorf("ARVADOS_API_HOST: %s", err)
			return 1
		}
		cluster.Services.Controller.ExternalURL = arvados.URL(*u)
		if i := os.Getenv("ARVADOS_API_HOST_INSECURE"); i != "" && i != "0" {
			cluster.TLS.Insecure = true
		}
	}

	handler := c.newHandler(ctx, cluster, profile, cluster.SystemRootToken)
	if err = handler.CheckHealth(); err != nil {
		return 1
	}
	srv := &httpserver.Server{
		Server: http.Server{
			Handler: httpserver.AddRequestIDs(httpserver.LogRequests(log, handler)),
		},
		Addr: listen,
	}
	err = srv.Start()
	if err != nil {
		return 1
	}
	log.WithFields(logrus.Fields{
		"Listen":  srv.Addr,
		"Service": c.svcName,
	}).Info("listening")
	if _, err := daemon.SdNotify(false, "READY=1"); err != nil {
		log.WithError(err).Errorf("error notifying init daemon")
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	err = srv.Wait()
	if err != nil {
		return 1
	}
	return 0
}

const rfc3339NanoFixed = "2006-01-02T15:04:05.000000000Z07:00"
