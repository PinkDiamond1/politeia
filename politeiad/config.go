// Copyright (c) 2013-2014 The btcsuite developers
// Copyright (c) 2015-2020 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/decred/dcrd/dcrutil/v3"
	v1 "github.com/decred/dcrtime/api/v1"
	"github.com/decred/politeia/politeiad/backendv2/tstorebe/tstore"
	"github.com/decred/politeia/util"
	"github.com/decred/politeia/util/version"
	flags "github.com/jessevdk/go-flags"
)

const (
	defaultConfigFilename   = "politeiad.conf"
	defaultDataDirname      = "data"
	defaultLogLevel         = "info"
	defaultLogDirname       = "logs"
	defaultLogFilename      = "politeiad.log"
	defaultIdentityFilename = "identity.json"

	defaultMainnetPort = "49374"
	defaultTestnetPort = "59374"

	defaultMainnetDcrdata = "dcrdata.decred.org:443"
	defaultTestnetDcrdata = "testnet.decred.org:443"

	// Backend options
	backendGit     = "git"
	backendTstore  = "tstore"
	defaultBackend = backendTstore

	// Tstore default settings
	defaultDBType   = tstore.DBTypeLevelDB
	defaultDBHost   = "localhost:3306" // MySQL default host
	defaultTlogHost = "localhost:8090"

	// Environment variables
	envDBPass = "DBPASS"
)

var (
	defaultHomeDir       = dcrutil.AppDataDir("politeiad", false)
	defaultConfigFile    = filepath.Join(defaultHomeDir, defaultConfigFilename)
	defaultDataDir       = filepath.Join(defaultHomeDir, defaultDataDirname)
	defaultHTTPSKeyFile  = filepath.Join(defaultHomeDir, "https.key")
	defaultHTTPSCertFile = filepath.Join(defaultHomeDir, "https.cert")
	defaultLogDir        = filepath.Join(defaultHomeDir, defaultLogDirname)
	defaultIdentityFile  = filepath.Join(defaultHomeDir, defaultIdentityFilename)

	// defaultReadTimeout is the maximum duration in seconds that is spent
	// reading the request headers and body.
	defaultReadTimeout int64 = 5

	// defaultWriteTimeout is the maximum duration in seconds that a request
	// connection is kept open.
	defaultWriteTimeout int64 = 60

	// defaultReqBodySizeLimit is the maximum number of bytes allowed in a
	// request body.
	defaultReqBodySizeLimit int64 = 3 * 1024 * 1024 // 3 MiB
)

// runServiceCommand is only set to a real function on Windows.  It is used
// to parse and execute service commands specified via the -s flag.
var runServiceCommand func(string) error

// config defines the configuration options for dcrd.
//
// See loadConfig for details on the configuration load process.
type config struct {
	HomeDir     string   `short:"A" long:"appdata" description:"Path to application home directory"`
	ShowVersion bool     `short:"V" long:"version" description:"Display version information and exit"`
	ConfigFile  string   `short:"C" long:"configfile" description:"Path to configuration file"`
	DataDir     string   `short:"b" long:"datadir" description:"Directory to store data"`
	LogDir      string   `long:"logdir" description:"Directory to log output."`
	TestNet     bool     `long:"testnet" description:"Use the test network"`
	SimNet      bool     `long:"simnet" description:"Use the simulation test network"`
	Profile     string   `long:"profile" description:"Enable HTTP profiling on given port -- NOTE port must be between 1024 and 65536"`
	CPUProfile  string   `long:"cpuprofile" description:"Write CPU profile to the specified file"`
	MemProfile  string   `long:"memprofile" description:"Write mem profile to the specified file"`
	DebugLevel  string   `short:"d" long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`
	Listeners   []string `long:"listen" description:"Add an interface/port to listen for connections (default all interfaces port: 49152, testnet: 59152)"`
	Version     string
	HTTPSCert   string `long:"httpscert" description:"File containing the https certificate file"`
	HTTPSKey    string `long:"httpskey" description:"File containing the https certificate key"`
	RPCUser     string `long:"rpcuser" description:"RPC user name for privileged commands"`
	RPCPass     string `long:"rpcpass" description:"RPC password for privileged commands"`
	DcrtimeHost string `long:"dcrtimehost" description:"Dcrtime ip:port"`
	DcrtimeCert string // Provided in env variable "DCRTIMECERT"
	Identity    string `long:"identity" description:"File containing the politeiad identity file"`
	Backend     string `long:"backend" description:"Backend type"`
	Fsck        bool   `long:"fsck" description:"Perform filesystem checks on all record and plugin data"`

	// Web server settings
	ReadTimeout      int64 `long:"readtimeout" description:"Maximum duration in seconds that is spent reading the request headers and body"`
	WriteTimeout     int64 `long:"writetimeout" description:"Maximum duration in seconds that a request connection is kept open"`
	ReqBodySizeLimit int64 `long:"reqbodysizelimit" description:"Maximum number of bytes allowed for a request body from a http client"`

	// Git backend options
	GitTrace    bool   `long:"gittrace" description:"Enable git tracing in logs"`
	DcrdataHost string `long:"dcrdatahost" description:"Dcrdata ip:port"`

	// Tstore backend options
	DBType   string `long:"dbtype" description:"Database type"`
	DBHost   string `long:"dbhost" description:"Database ip:port"`
	DBPass   string // Provided in env variable "DBPASS"
	TlogHost string `long:"tloghost" description:"Trillian log ip:port"`

	// Plugin options
	Plugins        []string `long:"plugin" description:"Plugins"`
	PluginSettings []string `long:"pluginsetting" description:"Plugin settings"`
}

// serviceOptions defines the configuration options for the daemon as a service
// on Windows.
type serviceOptions struct {
	ServiceCommand string `short:"s" long:"service" description:"Service command {install, remove, start, stop}"`
}

// validLogLevel returns whether or not logLevel is a valid debug log level.
func validLogLevel(logLevel string) bool {
	switch logLevel {
	case "trace":
		fallthrough
	case "debug":
		fallthrough
	case "info":
		fallthrough
	case "warn":
		fallthrough
	case "error":
		fallthrough
	case "critical":
		return true
	}
	return false
}

// supportedSubsystems returns a sorted slice of the supported subsystems for
// logging purposes.
func supportedSubsystems() []string {
	// Convert the subsystemLoggers map keys to a slice.
	subsystems := make([]string, 0, len(subsystemLoggers))
	for subsysID := range subsystemLoggers {
		subsystems = append(subsystems, subsysID)
	}

	// Sort the subsytems for stable display.
	sort.Strings(subsystems)
	return subsystems
}

// parseAndSetDebugLevels attempts to parse the specified debug level and set
// the levels accordingly.  An appropriate error is returned if anything is
// invalid.
func parseAndSetDebugLevels(debugLevel string) error {
	// When the specified string doesn't have any delimters, treat it as
	// the log level for all subsystems.
	if !strings.Contains(debugLevel, ",") && !strings.Contains(debugLevel, "=") {
		// Validate debug log level.
		if !validLogLevel(debugLevel) {
			str := "The specified debug level [%v] is invalid"
			return fmt.Errorf(str, debugLevel)
		}

		// Change the logging level for all subsystems.
		setLogLevels(debugLevel)

		return nil
	}

	// Split the specified string into subsystem/level pairs while detecting
	// issues and update the log levels accordingly.
	for _, logLevelPair := range strings.Split(debugLevel, ",") {
		if !strings.Contains(logLevelPair, "=") {
			str := "The specified debug level contains an invalid " +
				"subsystem/level pair [%v]"
			return fmt.Errorf(str, logLevelPair)
		}

		// Extract the specified subsystem and log level.
		fields := strings.Split(logLevelPair, "=")
		subsysID, logLevel := fields[0], fields[1]

		// Validate subsystem.
		if _, exists := subsystemLoggers[subsysID]; !exists {
			str := "The specified subsystem [%v] is invalid -- " +
				"supported subsytems %v"
			return fmt.Errorf(str, subsysID, supportedSubsystems())
		}

		// Validate log level.
		if !validLogLevel(logLevel) {
			str := "The specified debug level [%v] is invalid"
			return fmt.Errorf(str, logLevel)
		}

		setLogLevel(subsysID, logLevel)
	}

	return nil
}

// removeDuplicateAddresses returns a new slice with all duplicate entries in
// addrs removed.
func removeDuplicateAddresses(addrs []string) []string {
	result := make([]string, 0, len(addrs))
	seen := map[string]struct{}{}
	for _, val := range addrs {
		if _, ok := seen[val]; !ok {
			result = append(result, val)
			seen[val] = struct{}{}
		}
	}
	return result
}

// normalizeAddresses returns a new slice with all the passed peer addresses
// normalized with the given default port, and all duplicates removed.
func normalizeAddresses(addrs []string, defaultPort string) []string {
	for i, addr := range addrs {
		addrs[i] = util.NormalizeAddress(addr, defaultPort)
	}

	return removeDuplicateAddresses(addrs)
}

// newConfigParser returns a new command line flags parser.
func newConfigParser(cfg *config, so *serviceOptions, options flags.Options) *flags.Parser {
	parser := flags.NewParser(cfg, options)
	if runtime.GOOS == "windows" {
		parser.AddGroup("Service Options", "Service Options", so)
	}
	return parser
}

// loadConfig initializes and parses the config using a config file and command
// line options.
//
// The configuration proceeds as follows:
// 	1) Start with a default config with sane settings
// 	2) Pre-parse the command line to check for an alternative config file
// 	3) Load configuration file overwriting defaults with any specified options
// 	4) Parse CLI options and overwrite/add any specified options
//
// The above results in daemon functioning properly without any config settings
// while still allowing the user to override settings with config files and
// command line options.  Command line options always take precedence.
func loadConfig() (*config, []string, error) {
	// Default config.
	cfg := config{
		HomeDir:          defaultHomeDir,
		ConfigFile:       defaultConfigFile,
		DebugLevel:       defaultLogLevel,
		DataDir:          defaultDataDir,
		LogDir:           defaultLogDir,
		HTTPSKey:         defaultHTTPSKeyFile,
		HTTPSCert:        defaultHTTPSCertFile,
		Version:          version.String(),
		Backend:          defaultBackend,
		ReadTimeout:      defaultReadTimeout,
		WriteTimeout:     defaultWriteTimeout,
		ReqBodySizeLimit: defaultReqBodySizeLimit,
		DBType:           defaultDBType,
		DBHost:           defaultDBHost,
		TlogHost:         defaultTlogHost,
	}

	// Service options which are only added on Windows.
	serviceOpts := serviceOptions{}

	// Pre-parse the command line options to see if an alternative config
	// file or the version flag was specified.  Any errors aside from the
	// help message error can be ignored here since they will be caught by
	// the final parse below.
	preCfg := cfg
	preParser := newConfigParser(&preCfg, &serviceOpts, flags.HelpFlag)
	_, err := preParser.Parse()
	if err != nil {
		var e *flags.Error
		if errors.As(err, &e) && e.Type == flags.ErrHelp {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(0)
		}
	}

	// Show the version and exit if the version flag was specified.
	appName := filepath.Base(os.Args[0])
	appName = strings.TrimSuffix(appName, filepath.Ext(appName))
	usageMessage := fmt.Sprintf("Use %s -h to show usage", appName)
	if preCfg.ShowVersion {
		fmt.Printf("%s version %s (Go version %s %s/%s)\n", appName,
			version.String(), runtime.Version(), runtime.GOOS,
			runtime.GOARCH)
		os.Exit(0)
	}

	// Perform service command and exit if specified.  Invalid service
	// commands show an appropriate error.  Only runs on Windows since
	// the runServiceCommand function will be nil when not on Windows.
	if serviceOpts.ServiceCommand != "" && runServiceCommand != nil {
		err := runServiceCommand(serviceOpts.ServiceCommand)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(0)
	}

	// Update the home directory for stakepoold if specified. Since the
	// home directory is updated, other variables need to be updated to
	// reflect the new changes.
	if preCfg.HomeDir != "" {
		cfg.HomeDir, _ = filepath.Abs(preCfg.HomeDir)

		if preCfg.ConfigFile == defaultConfigFile {
			cfg.ConfigFile = filepath.Join(cfg.HomeDir, defaultConfigFilename)
		} else {
			cfg.ConfigFile = preCfg.ConfigFile
		}
		if preCfg.DataDir == defaultDataDir {
			cfg.DataDir = filepath.Join(cfg.HomeDir, defaultDataDirname)
		} else {
			cfg.DataDir = preCfg.DataDir
		}
		if preCfg.HTTPSKey == defaultHTTPSKeyFile {
			cfg.HTTPSKey = filepath.Join(cfg.HomeDir, "https.key")
		} else {
			cfg.HTTPSKey = preCfg.HTTPSKey
		}
		if preCfg.HTTPSCert == defaultHTTPSCertFile {
			cfg.HTTPSCert = filepath.Join(cfg.HomeDir, "https.cert")
		} else {
			cfg.HTTPSCert = preCfg.HTTPSCert
		}
		if preCfg.LogDir == defaultLogDir {
			cfg.LogDir = filepath.Join(cfg.HomeDir, defaultLogDirname)
		} else {
			cfg.LogDir = preCfg.LogDir
		}
	}

	// Load additional config from file.
	var configFileError error
	parser := newConfigParser(&cfg, &serviceOpts, flags.Default)
	if !(preCfg.SimNet) || cfg.ConfigFile != defaultConfigFile {
		err := flags.NewIniParser(parser).ParseFile(cfg.ConfigFile)
		if err != nil {
			var e *os.PathError
			if !errors.As(err, &e) {
				fmt.Fprintf(os.Stderr, "Error parsing config "+
					"file: %v\n", err)
				fmt.Fprintln(os.Stderr, usageMessage)
				return nil, nil, err
			}
			configFileError = err
		}
	}

	// Parse command line options again to ensure they take precedence.
	remainingArgs, err := parser.Parse()
	if err != nil {
		var e *flags.Error
		if !errors.As(err, &e) || e.Type != flags.ErrHelp {
			fmt.Fprintln(os.Stderr, usageMessage)
		}
		return nil, nil, err
	}

	// Create the home directory if it doesn't already exist.
	funcName := "loadConfig"
	err = os.MkdirAll(defaultHomeDir, 0700)
	if err != nil {
		// Show a nicer error message if it's because a symlink is
		// linked to a directory that does not exist (probably because
		// it's not mounted).
		var e *os.PathError
		if errors.As(err, &e) && os.IsExist(err) {
			if link, lerr := os.Readlink(e.Path); lerr == nil {
				str := "is symlink %s -> %s mounted?"
				err = fmt.Errorf(str, e.Path, link)
			}
		}

		str := "%s: Failed to create home directory: %v"
		err := fmt.Errorf(str, funcName, err)
		fmt.Fprintln(os.Stderr, err)
		return nil, nil, err
	}

	// Multiple networks can't be selected simultaneously.
	numNets := 0

	// Count number of network flags passed; assign active network params
	// while we're at it
	port := defaultMainnetPort
	activeNetParams = &mainNetParams
	if cfg.TestNet {
		numNets++
		activeNetParams = &testNet3Params
		port = defaultTestnetPort
	}
	if cfg.SimNet {
		numNets++
		// Also disable dns seeding on the simulation test network.
		activeNetParams = &simNetParams
	}
	if numNets > 1 {
		str := "%s: The testnet and simnet params can't be " +
			"used together -- choose one of the three"
		err := fmt.Errorf(str, funcName)
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, usageMessage)
		return nil, nil, err
	}

	// Append the network type to the data directory so it is "namespaced"
	// per network.  In addition to the block database, there are other
	// pieces of data that are saved to disk such as address manager state.
	// All data is specific to a network, so namespacing the data directory
	// means each individual piece of serialized data does not have to
	// worry about changing names per network and such.
	cfg.DataDir = util.CleanAndExpandPath(cfg.DataDir)
	cfg.DataDir = filepath.Join(cfg.DataDir, netName(activeNetParams))

	// Append the network type to the log directory so it is "namespaced"
	// per network in the same fashion as the data directory.
	cfg.LogDir = util.CleanAndExpandPath(cfg.LogDir)
	cfg.LogDir = filepath.Join(cfg.LogDir, netName(activeNetParams))

	cfg.HTTPSKey = util.CleanAndExpandPath(cfg.HTTPSKey)
	cfg.HTTPSCert = util.CleanAndExpandPath(cfg.HTTPSCert)

	// Special show command to list supported subsystems and exit.
	if cfg.DebugLevel == "show" {
		fmt.Println("Supported subsystems", supportedSubsystems())
		os.Exit(0)
	}

	// Initialize log rotation.  After log rotation has been initialized,
	// the logger variables may be used.
	initLogRotator(filepath.Join(cfg.LogDir, defaultLogFilename))

	// Parse, validate, and set debug log level(s).
	if err := parseAndSetDebugLevels(cfg.DebugLevel); err != nil {
		err := fmt.Errorf("%s: %v", funcName, err.Error())
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, usageMessage)
		return nil, nil, err
	}

	// Validate profile port number
	if cfg.Profile != "" {
		profilePort, err := strconv.Atoi(cfg.Profile)
		if err != nil || profilePort < 1024 || profilePort > 65535 {
			str := "%s: The profile port must be between 1024 and 65535"
			err := fmt.Errorf(str, funcName)
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprintln(os.Stderr, usageMessage)
			return nil, nil, err
		}
	}

	// Add the default listener if none were specified. The default
	// listener is all addresses on the listen port for the network
	// we are to connect to.
	if len(cfg.Listeners) == 0 {
		cfg.Listeners = []string{
			net.JoinHostPort("", port),
		}
	}

	// Add default port to all listener addresses if needed and remove
	// duplicate addresses.
	cfg.Listeners = normalizeAddresses(cfg.Listeners, port)

	if len(cfg.DcrdataHost) == 0 {
		if cfg.TestNet {
			cfg.DcrdataHost = defaultTestnetDcrdata
		} else {
			cfg.DcrdataHost = defaultMainnetDcrdata
		}
	}
	cfg.DcrdataHost = "https://" + cfg.DcrdataHost

	if cfg.TestNet {
		var timeHost string
		if len(cfg.DcrtimeHost) == 0 {
			timeHost = v1.DefaultTestnetTimeHost
		} else {
			timeHost = cfg.DcrtimeHost
		}
		cfg.DcrtimeHost = util.NormalizeAddress(timeHost,
			v1.DefaultTestnetTimePort)
	} else {
		var timeHost string
		if len(cfg.DcrtimeHost) == 0 {
			timeHost = v1.DefaultMainnetTimeHost
		} else {
			timeHost = cfg.DcrtimeHost
		}
		cfg.DcrtimeHost = util.NormalizeAddress(timeHost,
			v1.DefaultMainnetTimePort)
	}
	cfg.DcrtimeHost = "https://" + cfg.DcrtimeHost

	if len(cfg.DcrtimeCert) != 0 && !util.FileExists(cfg.DcrtimeCert) {
		cfg.DcrtimeCert = util.CleanAndExpandPath(cfg.DcrtimeCert)
		path := filepath.Join(cfg.HomeDir, cfg.DcrtimeCert)
		if !util.FileExists(path) {
			str := "%s: dcrtimecert " + cfg.DcrtimeCert + " and " +
				path + " don't exist"
			err := fmt.Errorf(str, funcName)
			fmt.Fprintln(os.Stderr, err)
			return nil, nil, err
		}

		cfg.DcrtimeCert = path
	}

	if cfg.Identity == "" {
		cfg.Identity = defaultIdentityFile
	}
	cfg.Identity = util.CleanAndExpandPath(cfg.Identity)

	// Set random username and password when not specified
	if cfg.RPCUser == "" {
		name, err := util.Random(32)
		if err != nil {
			return nil, nil, err
		}
		cfg.RPCUser = base64.StdEncoding.EncodeToString(name)
		log.Warnf("RPC user name not set, using random value")
	}
	if cfg.RPCPass == "" {
		pass, err := util.Random(32)
		if err != nil {
			return nil, nil, err
		}
		cfg.RPCPass = base64.StdEncoding.EncodeToString(pass)
		log.Warnf("RPC password not set, using random value")
	}

	// Verify backend specific settings
	switch cfg.Backend {
	case backendGit:
		// Nothing to do
	case backendTstore:
		err = verifyTstoreSettings(&cfg)
		if err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("invalid backend type '%v'", cfg.Backend)
	}

	// Warn about missing config file only after all other configuration is
	// done.  This prevents the warning on help messages and invalid
	// options.  Note this should go directly before the return.
	if configFileError != nil {
		log.Warnf("%v", configFileError)
	}

	return &cfg, remainingArgs, nil
}

// verifyTstoreSettings verifies the config settings that are specific to the
// tstore backend.
func verifyTstoreSettings(cfg *config) error {
	// Verify tstore backend database choice
	switch cfg.DBType {
	case tstore.DBTypeLevelDB:
		// Allowed; continue
	case tstore.DBTypeMySQL:
		// The database password is provided in an env variable
		cfg.DBPass = os.Getenv(envDBPass)
		if cfg.DBPass == "" {
			return fmt.Errorf("dbpass not found; you must provide the " +
				"database password for the politeiad user in the env " +
				"variable DBPASS")
		}
	}

	// Verify tlog options
	_, err := url.Parse(cfg.TlogHost)
	if err != nil {
		return fmt.Errorf("invalid tlog host '%v': %v", cfg.TlogHost, err)
	}

	return nil
}
