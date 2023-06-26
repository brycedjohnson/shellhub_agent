package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/kelseyhightower/envconfig"
	"github.com/brycedjohnson/shellhub-agent/pkg/tunnel"
	
	"github.com/brycedjohnson/shellhub-agent/server"
	"github.com/brycedjohnson/shellhub-agent/pkg/loglevel"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// AgentVersion store the version to be embed inside the binary. This is
// injected using `-ldflags` build option (e.g: `go build -ldflags "-X
// main.AgentVersion=1.2.3"`).
//
// If set to `latest`, the auto-updating mechanism is disabled. This is intended
// to be used during development only.
var AgentVersion string

// ConfigOptions provides the configuration for the agent service. The values are load from
// the system environment and control multiple aspects of the service.
type ConfigOptions struct {
	// Set the ShellHub Cloud server address the agent will use to connect.
	ServerAddress string `envconfig:"server_address" required:"true"`

	// Specify the path to the device private key.
	PrivateKey string `envconfig:"private_key" required:"true"`

	// Sets the account tenant id used during communication to associate the
	// device to a specific tenant.
	TenantID string `envconfig:"tenant_id" required:"true"`

	// Determine the interval to send the keep alive message to the server. This
	// has a direct impact of the bandwidth used by the device when in idle
	// state. Default is 30 seconds.
	KeepAliveInterval int `envconfig:"keepalive_interval" default:"30"`

	// Set the device preferred hostname. This provides a hint to the server to
	// use this as hostname if it is available.
	PreferredHostname string `envconfig:"preferred_hostname"`

	// Set the device preferred identity. This provides a hint to the server to
	// use this identity if it is available.
	PreferredIdentity string `envconfig:"preferred_identity" default:""`

	// Set password for single-user mode (without root privileges). If not provided,
	// multi-user mode (with root privileges) is enabled by default.
	// NOTE: The password hash could be generated by ```openssl passwd```.
	SingleUserPassword string `envconfig:"simple_user_password"`

	// Log level to use. Valid values are 'info', 'warning', 'error', 'debug', and 'trace'.
	LogLevel string `envconfig:"log_level" default:"info"`
}

// NewAgentServer creates a new agent server instance.
func NewAgentServer() *Agent { // nolint:gocyclo
	opts := ConfigOptions{}

	// Process unprefixed env vars for backward compatibility
	envconfig.Process("", &opts) // nolint:errcheck

	if err := envconfig.Process("shellhub", &opts); err != nil {
		// show envconfig usage help users to run agent
		envconfig.Usage("shellhub", &opts) // nolint:errcheck
		log.Fatal(err)
	}

	// Set the log level accordingly to the configuration.
	level, err := log.ParseLevel(opts.LogLevel)
	if err != nil {
		log.Error("Invalid log level has been provided.")
		os.Exit(1)
	}
	log.SetLevel(level)

	if os.Geteuid() == 0 && opts.SingleUserPassword != "" {
		log.Error("ShellHub agent cannot run as root when single-user mode is enabled.")
		log.Error("To disable single-user mode unset SHELLHUB_SINGLE_USER_PASSWORD env.")
		os.Exit(1)
	}

	if os.Geteuid() != 0 && opts.SingleUserPassword == "" {
		log.Error("When running as non-root user you need to set password for single-user mode by SHELLHUB_SINGLE_USER_PASSWORD environment variable.")
		log.Error("You can use openssl passwd utility to generate password hash. The following algorithms are supported: bsd1, apr1, sha256, sha512.")
		log.Error("Example: SHELLHUB_SINGLE_USER_PASSWORD=$(openssl passwd -6)")
		log.Error("See man openssl-passwd for more information.")
		os.Exit(1)
	}

	
	log.WithFields(log.Fields{
		"version": AgentVersion,
		"mode": func() string {
			if opts.SingleUserPassword != "" {
				return "single-user"
			}

			return "multi-user"
		}(),
	}).Info("Starting ShellHub")

	agent, err := NewAgent(&opts)
	if err != nil {
		log.Fatal(err)
	}

	if err := agent.initialize(); err != nil {
		log.WithFields(log.Fields{"err": err}).Fatal("Failed to initialize agent")
	}

	serv := server.NewServer(agent.cli, agent.authData, opts.PrivateKey, opts.KeepAliveInterval, opts.SingleUserPassword)

	tun := tunnel.NewTunnel()
	tun.ConnHandler = func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "webserver doesn't support hijacking", http.StatusInternalServerError)

			return
		}

		if _, _, err := hj.Hijack(); err != nil {
			http.Error(w, "failed to hijack connection", http.StatusInternalServerError)

			return
		}

		vars := mux.Vars(r)
		conn, ok := r.Context().Value("http-conn").(net.Conn)
		if !ok {
			log.WithFields(log.Fields{
				"version": AgentVersion,
			}).Warning("Type assertion failed")

			return
		}

		serv.Sessions[vars["id"]] = conn
		serv.HandleConn(conn)

		conn.Close()
	}
	tun.HTTPHandler = func(w http.ResponseWriter, r *http.Request) {
		replyError := func(err error, msg string, code int) {
			log.WithError(err).WithFields(log.Fields{
				"remote":    r.RemoteAddr,
				"namespace": r.Header.Get("X-Namespace"),
				"path":      r.Header.Get("X-Path"),
				"version":   AgentVersion,
			}).Error(msg)

			http.Error(w, msg, code)
		}

		in, err := net.Dial("tcp", ":80")
		if err != nil {
			replyError(err, "failed to connect to HTTP the server on device", http.StatusInternalServerError)

			return
		}

		defer in.Close()

		url, err := r.URL.Parse(r.Header.Get("X-Path"))
		if err != nil {
			replyError(err, "failed to parse URL", http.StatusInternalServerError)

			return
		}

		r.URL.Scheme = "http"
		r.URL = url

		if err := r.Write(in); err != nil {
			replyError(err, "failed to write request to the server on device", http.StatusInternalServerError)

			return
		}

		ctr := http.NewResponseController(w)
		out, _, err := ctr.Hijack()
		if err != nil {
			replyError(err, "failed to hijack connection", http.StatusInternalServerError)

			return
		}

		defer out.Close() // nolint:errcheck

		if _, err := io.Copy(out, in); errors.Is(err, io.ErrUnexpectedEOF) {
			replyError(err, "failed to copy response from device service to client", http.StatusInternalServerError)

			return
		}
	}
	tun.CloseHandler = func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		serv.CloseSession(vars["id"])
	}

	serv.SetDeviceName(agent.authData.Name)

	go func() {
		for {
			listener, err := agent.newReverseListener()
			if err != nil {
				time.Sleep(time.Second * 10)

				continue
			}

			namespace := agent.authData.Namespace
			tenantName := agent.authData.Name
			sshEndpoint := agent.serverInfo.Endpoints.SSH

			sshid := strings.NewReplacer(
				"{namespace}", namespace,
				"{tenantName}", tenantName,
				"{sshEndpoint}", strings.Split(sshEndpoint, ":")[0],
			).Replace("{namespace}.{tenantName}@{sshEndpoint}")

			log.WithFields(log.Fields{
				"namespace":      namespace,
				"hostname":       tenantName,
				"server_address": opts.ServerAddress,
				"ssh_server":     sshEndpoint,
				"sshid":          sshid,
			}).Info("Server connection established")

			if err := tun.Listen(listener); err != nil {
				continue
			}
		}
	}()

	// This hard coded interval will be removed in a follow up change to make use of JWT token expire time.
	ticker := time.NewTicker(10 * time.Minute)

	for range ticker.C {
		sessions := make([]string, 0, len(serv.Sessions))
		for key := range serv.Sessions {
			sessions = append(sessions, key)
		}

		agent.sessions = sessions

		if err := agent.authorize(); err != nil {
			serv.SetDeviceName(agent.authData.Name)
		}
	}

	return agent
}

func main() {
	// Default command.
	rootCmd := &cobra.Command{ // nolint: exhaustruct
		Use: "agent",
		Run: func(cmd *cobra.Command, args []string) {
			loglevel.SetLogLevel()

			NewAgentServer()
		},
	}

	rootCmd.AddCommand(&cobra.Command{ // nolint: exhaustruct
		Use:   "info",
		Short: "Show information about the agent",
		Run: func(cmd *cobra.Command, args []string) {
			loglevel.SetLogLevel()

			if err := NewAgentServer().probeServerInfo(); err != nil {
				log.Fatal(err)
			}
		},
	})

	rootCmd.AddCommand(&cobra.Command{ // nolint: exhaustruct
		Use:   "sftp",
		Short: "Starts the SFTP server",
		Long: `Starts the SFTP server. This command is used internally by the agent and should not be used directly.
It is initialized by the agent when a new SFTP session is created.`,
		Run: func(cmd *cobra.Command, args []string) {
			NewSFTPServer()
		},
	})

	rootCmd.Version = AgentVersion

	rootCmd.SetVersionTemplate(fmt.Sprintf("{{ .Name }} version: {{ .Version }}\ngo: %s\n",
		runtime.Version(),
	))

	rootCmd.Execute() // nolint: errcheck
}
