// Copyright (c) 2017-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"
	v1 "github.com/decred/politeia/politeiad/api/v1"
	"github.com/decred/politeia/politeiad/api/v1/identity"
	v2 "github.com/decred/politeia/politeiad/api/v2"
	"github.com/decred/politeia/politeiad/backend"
	"github.com/decred/politeia/politeiad/backend/gitbe"
	"github.com/decred/politeia/politeiad/backendv2"
	"github.com/decred/politeia/politeiad/backendv2/tstorebe"
	"github.com/decred/politeia/util"
	"github.com/decred/politeia/util/version"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
)

type permission uint

const (
	permissionPublic permission = iota
	permissionAuth
)

// politeia application context.
type politeia struct {
	backend   backend.Backend
	backendv2 backendv2.Backend
	cfg       *config
	router    *mux.Router
	identity  *identity.FullIdentity
}

func remoteAddr(r *http.Request) string {
	via := r.RemoteAddr
	xff := r.Header.Get(v1.Forward)
	if xff != "" {
		return fmt.Sprintf("%v via %v", xff, r.RemoteAddr)
	}
	return via
}

// handleNotFound is a generic handler for an invalid route.
func (p *politeia) handleNotFound(w http.ResponseWriter, r *http.Request) {
	// Log incoming connection
	log.Debugf("Invalid route: %v %v %v %v", remoteAddr(r), r.Method, r.URL,
		r.Proto)

	// Trace incoming request
	log.Tracef("%v", newLogClosure(func() string {
		trace, err := httputil.DumpRequest(r, true)
		if err != nil {
			trace = []byte(fmt.Sprintf("logging: "+
				"DumpRequest %v", err))
		}
		return string(trace)
	}))

	util.RespondWithJSON(w, http.StatusNotFound, v1.ServerErrorReply{})
}

func (p *politeia) respondWithUserError(w http.ResponseWriter, errorCode v1.ErrorStatusT, errorContext []string) {
	util.RespondWithJSON(w, http.StatusBadRequest, v1.UserErrorReply{
		ErrorCode:    errorCode,
		ErrorContext: errorContext,
	})
}

func (p *politeia) respondWithServerError(w http.ResponseWriter, errorCode int64, err error) {
	// If this is a pkg/errors error then we can pull the
	// stack trace out of the error, otherwise, we use the
	// stack trace for this function.
	stack, ok := util.StackTrace(err)
	if !ok {
		stack = string(debug.Stack())
	}

	log.Errorf("Stacktrace (NOT A REAL CRASH): %v", stack)

	util.RespondWithJSON(w, http.StatusInternalServerError, v1.ServerErrorReply{
		ErrorCode: errorCode,
	})
}

func (p *politeia) check(user, pass string) bool {
	if user != p.cfg.RPCUser || pass != p.cfg.RPCPass {
		return false
	}
	return true
}

func (p *politeia) auth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !p.check(user, pass) {
			log.Infof("%v Unauthorized access for: %v",
				remoteAddr(r), user)
			w.Header().Set("WWW-Authenticate",
				`Basic realm="Politeiad"`)
			w.WriteHeader(401)
			p.respondWithUserError(w, v1.ErrorStatusInvalidRPCCredentials, nil)
			return
		}
		log.Infof("%v Authorized access for: %v",
			remoteAddr(r), user)
		fn(w, r)
	}
}

func (p *politeia) addRoute(method string, route string, handler http.HandlerFunc, perm permission) {
	if perm == permissionAuth {
		handler = p.auth(handler)
	}
	p.router.StrictSlash(true).HandleFunc(route, handler).Methods(method)
}

func (p *politeia) addRouteV2(method string, route string, handler http.HandlerFunc, perm permission) {
	route = v2.APIRoute + route
	p.addRoute(method, route, handler, perm)
}

func (p *politeia) setupBackendGit(anp *chaincfg.Params) error {
	if p.router == nil {
		return errors.Errorf("router must be initialized")
	}

	b, err := gitbe.New(anp, p.cfg.DataDir, p.cfg.DcrtimeHost,
		"", p.identity, p.cfg.GitTrace, p.cfg.DcrdataHost)
	if err != nil {
		return fmt.Errorf("new gitbe: %v", err)
	}
	p.backend = b

	// Not found
	p.router.NotFoundHandler = http.HandlerFunc(p.handleNotFound)

	// Unprivileged routes
	p.addRoute(http.MethodPost, v1.IdentityRoute, p.getIdentity,
		permissionPublic)
	p.addRoute(http.MethodPost, v1.NewRecordRoute, p.newRecord,
		permissionPublic)
	p.addRoute(http.MethodPost, v1.UpdateUnvettedRoute, p.updateUnvetted,
		permissionPublic)
	p.addRoute(http.MethodPost, v1.UpdateVettedRoute, p.updateVetted,
		permissionPublic)
	p.addRoute(http.MethodPost, v1.GetUnvettedRoute, p.getUnvetted,
		permissionPublic)
	p.addRoute(http.MethodPost, v1.GetVettedRoute, p.getVetted,
		permissionPublic)

	// Routes that require auth
	p.addRoute(http.MethodPost, v1.InventoryRoute, p.inventory,
		permissionAuth)
	p.addRoute(http.MethodPost, v1.SetUnvettedStatusRoute,
		p.setUnvettedStatus, permissionAuth)
	p.addRoute(http.MethodPost, v1.SetVettedStatusRoute,
		p.setVettedStatus, permissionAuth)
	p.addRoute(http.MethodPost, v1.UpdateVettedMetadataRoute,
		p.updateVettedMetadata, permissionAuth)

	// Set plugin routes. Requires auth.
	p.addRoute(http.MethodPost, v1.PluginCommandRoute, p.pluginCommand,
		permissionAuth)
	p.addRoute(http.MethodPost, v1.PluginInventoryRoute, p.pluginInventory,
		permissionAuth)

	return nil
}

var (
	// regexpPluginSettingMulti matches against the plugin setting
	// value when it contains multiple values.
	//
	// pluginID,key,["value1","value2"] matches ["value1","value2"]
	regexpPluginSettingMulti = regexp.MustCompile(`(\[.*\]$)`)
)

// parsePluginSetting parses a politeiad config plugin setting. Plugin settings
// will be in following format. The value may be a single value or an array of
// values.
//
// pluginID,key,value
// pluginID,key,["value1","value2","value3"...]
//
// When multiple values are provided, the values must be formatted as a JSON
// encoded []string.
func parsePluginSetting(setting string) (string, *backendv2.PluginSetting, error) {
	formatMsg := `expected plugin setting format is ` +
		`pluginID,key,value OR pluginID,key,["value1","value2","value3"]`

	// Parse the plugin setting
	var (
		parsed = strings.Split(setting, ",")

		// isMulti indicates whether the plugin setting contains
		// multiple values. If the setting only contains a single
		// value then isMulti will be false.
		isMulti = regexpPluginSettingMulti.MatchString(setting)
	)
	switch {
	case len(parsed) < 3:
		return "", nil, errors.Errorf("missing csv entry '%v'; %v",
			setting, formatMsg)
	case len(parsed) == 3:
		// This is expected; continue
	case len(parsed) > 3 && isMulti:
		// This is expected; continue
	default:
		return "", nil, errors.Errorf("invalid format '%v'; %v",
			setting, formatMsg)
	}

	var (
		pluginID     = parsed[0]
		settingKey   = parsed[1]
		settingValue = parsed[2]
	)

	// Clean the strings. The setting value is allowed to be case
	// sensitive.
	pluginID = strings.ToLower(strings.TrimSpace(pluginID))
	settingKey = strings.ToLower(strings.TrimSpace(settingKey))
	settingValue = strings.TrimSpace(settingValue)

	// Handle multiple values
	if isMulti {
		// Parse values
		values := regexpPluginSettingMulti.FindString(setting)

		// Verify the values are formatted as valid JSON
		var s []string
		err := json.Unmarshal([]byte(values), &s)
		if err != nil {
			return "", nil, err
		}

		// Re-encode the JSON. This will remove any funny
		// formatting like whitespaces.
		b, err := json.Marshal(s)
		if err != nil {
			return "", nil, err
		}

		// Save the value
		settingValue = string(b)
	}

	return pluginID, &backendv2.PluginSetting{
		Key:   settingKey,
		Value: settingValue,
	}, nil
}

func (p *politeia) setupBackendTstore(anp *chaincfg.Params) error {
	if p.router == nil {
		return errors.Errorf("router must be initialized")
	}

	b, err := tstorebe.New(p.cfg.HomeDir, p.cfg.DataDir, anp,
		p.cfg.TlogHost, p.cfg.DBType, p.cfg.DBHost,
		p.cfg.DBPass, p.cfg.DcrtimeHost, p.cfg.DcrtimeCert)
	if err != nil {
		return fmt.Errorf("new tstorebe: %v", err)
	}
	p.backendv2 = b

	// Setup not found handler
	p.router.NotFoundHandler = http.HandlerFunc(p.handleNotFound)

	// Setup v1 routes
	p.addRoute(http.MethodPost, v1.IdentityRoute,
		p.getIdentity, permissionPublic)

	// Setup v2 routes
	p.addRouteV2(http.MethodPost, v2.RouteRecordNew,
		p.handleRecordNew, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RouteRecordEdit,
		p.handleRecordEdit, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RouteRecordEditMetadata,
		p.handleRecordEditMetadata, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RouteRecordSetStatus,
		p.handleRecordSetStatus, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RouteRecords,
		p.handleRecords, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RouteRecordTimestamps,
		p.handleRecordTimestamps, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RouteInventory,
		p.handleInventory, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RouteInventoryOrdered,
		p.handleInventoryOrdered, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RoutePluginWrite,
		p.handlePluginWrite, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RoutePluginReads,
		p.handlePluginReads, permissionPublic)
	p.addRouteV2(http.MethodPost, v2.RoutePluginInventory,
		p.handlePluginInventory, permissionPublic)

	p.addRouteV2(http.MethodPost, v2.RoutePluginInventory,
		p.handlePluginInventory, permissionPublic)

	// Setup plugins
	if len(p.cfg.Plugins) > 0 {
		// Parse plugin settings
		settings := make(map[string][]backendv2.PluginSetting)
		for _, v := range p.cfg.PluginSettings {
			// Parse plugin setting
			pluginID, ps, err := parsePluginSetting(v)
			if err != nil {
				return err
			}

			// Add to settings list
			pss, ok := settings[pluginID]
			if !ok {
				pss = make([]backendv2.PluginSetting, 0, 16)
			}
			pss = append(pss, *ps)

			// Save settings list
			settings[pluginID] = pss
		}

		// Register plugins
		for _, v := range p.cfg.Plugins {
			// Setup plugin
			ps, ok := settings[v]
			if !ok {
				ps = make([]backendv2.PluginSetting, 0)
			}
			plugin := backendv2.Plugin{
				ID:       v,
				Settings: ps,
				Identity: p.identity,
			}

			// Register plugin
			log.Infof("Register plugin: %v", v)
			err = p.backendv2.PluginRegister(plugin)
			if err != nil {
				return fmt.Errorf("PluginRegister %v: %v", v, err)
			}
		}

		// Setup plugins
		for _, v := range p.backendv2.PluginInventory() {
			log.Infof("Setup plugin: %v", v.ID)
			err = p.backendv2.PluginSetup(v.ID)
			if err != nil {
				return fmt.Errorf("plugin setup %v: %v", v.ID, err)
			}
		}
	}

	// Perform filesytem check
	if p.cfg.Fsck {
		err = p.backendv2.Fsck()
		if err != nil {
			return err
		}
	}

	return nil
}

func _main() error {
	// Load configuration and parse command line.  This function also
	// initializes logging and configures it accordingly.
	cfg, _, err := loadConfig()
	if err != nil {
		return fmt.Errorf("Could not load configuration file: %v", err)
	}
	defer func() {
		if logRotator != nil {
			logRotator.Close()
		}
	}()

	log.Infof("Version : %v", version.String())
	log.Infof("Build   : %v", version.BuildMainVersion())
	log.Infof("Network : %v", activeNetParams.Params.Name)
	log.Infof("Home dir: %v", cfg.HomeDir)

	// Create the data directory in case it does not exist.
	err = os.MkdirAll(cfg.DataDir, 0700)
	if err != nil {
		return err
	}

	// Generate the TLS cert and key file if both don't already
	// exist.
	if !util.FileExists(cfg.HTTPSKey) &&
		!util.FileExists(cfg.HTTPSCert) {
		log.Infof("Generating HTTPS keypair...")

		err := util.GenCertPair(elliptic.P521(), "politeiad",
			cfg.HTTPSCert, cfg.HTTPSKey)
		if err != nil {
			return fmt.Errorf("unable to create https keypair: %v",
				err)
		}

		log.Infof("HTTPS keypair created...")
	}

	// Generate ed25519 identity to save messages, tokens etc.
	if !util.FileExists(cfg.Identity) {
		log.Infof("Generating signing identity...")
		id, err := identity.New()
		if err != nil {
			return err
		}
		err = id.Save(cfg.Identity)
		if err != nil {
			return err
		}
		log.Infof("Signing identity created...")
	}

	// Setup the router. Middleware is executed in
	// the same order that they are registered in.
	router := mux.NewRouter()
	m := middleware{
		reqBodySizeLimit: cfg.ReqBodySizeLimit,
	}
	router.Use(closeBodyMiddleware) // MUST be registered first
	router.Use(m.reqBodySizeLimitMiddleware)
	router.Use(loggingMiddleware)
	router.Use(recoverMiddleware)

	// Setup application context.
	p := &politeia{
		cfg:    cfg,
		router: router,
	}

	// Load identity.
	p.identity, err = identity.LoadFullIdentity(cfg.Identity)
	if err != nil {
		return err
	}
	log.Infof("Public key: %x", p.identity.Public.Key)

	// Load certs, if there.  If they aren't there assume OS is used to
	// resolve cert validity.
	if len(cfg.DcrtimeCert) != 0 {
		var certPool *x509.CertPool
		if !util.FileExists(cfg.DcrtimeCert) {
			return fmt.Errorf("unable to find dcrtime cert %v",
				cfg.DcrtimeCert)
		}
		dcrtimeCert, err := ioutil.ReadFile(cfg.DcrtimeCert)
		if err != nil {
			return fmt.Errorf("unable to read dcrtime cert %v: %v",
				cfg.DcrtimeCert, err)
		}
		certPool = x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(dcrtimeCert) {
			return fmt.Errorf("unable to load cert")
		}
	}

	// Setup backend
	log.Infof("Backend: %v", cfg.Backend)
	switch cfg.Backend {
	case backendGit:
		err := p.setupBackendGit(activeNetParams.Params)
		if err != nil {
			return err
		}
	case backendTstore:
		err := p.setupBackendTstore(activeNetParams.Params)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid backend selected: %v", cfg.Backend)
	}

	// Bind to a port and pass our router in
	listenC := make(chan error)
	for _, listener := range cfg.Listeners {
		listen := listener
		go func() {
			s := &http.Server{
				Handler:      p.router,
				Addr:         listen,
				ReadTimeout:  time.Duration(cfg.ReadTimeout) * time.Second,
				WriteTimeout: time.Duration(cfg.WriteTimeout) * time.Second,
			}

			log.Infof("Listen: %v", listen)
			listenC <- s.ListenAndServeTLS(cfg.HTTPSCert, cfg.HTTPSKey)
		}()
	}

	// Tell user we are ready to go.
	log.Infof("Start of day")

	// Setup OS signals
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGINT)
	for {
		select {
		case sig := <-sigs:
			log.Infof("Terminating with %v", sig)
			goto done
		case err := <-listenC:
			log.Errorf("%v", err)
			goto done
		}
	}
done:
	switch p.cfg.Backend {
	case backendGit:
		p.backend.Close()
	case backendTstore:
		p.backendv2.Close()
	}

	log.Infof("Exiting")

	return nil
}

func main() {
	err := _main()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
