package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

var dataHome = path.Join(xdg.DataHome, "smallweb")
var sandboxPath = path.Join(dataHome, "sandbox.ts")

//go:embed deno/sandbox.ts
var sandboxBytes []byte

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func denoExecutable() (string, error) {
	if env, ok := os.LookupEnv("DENO_EXEC_PATH"); ok {
		return env, nil
	}

	return exec.LookPath("deno")
}

var extensions = []string{".js", ".ts", ".jsx", ".tsx"}

func inferEntrypoint(rootDir, alias string) (string, error) {
	for _, ext := range extensions {
		entrypoint := path.Join(rootDir, alias+ext)
		if exists(entrypoint) {
			return entrypoint, nil
		}
	}

	for _, ext := range extensions {
		entrypoint := path.Join(rootDir, alias, "mod"+ext)
		if exists(entrypoint) {
			return entrypoint, nil
		}
	}

	for _, ext := range extensions {
		entrypoint := path.Join(rootDir, alias, alias+ext)
		if exists(entrypoint) {
			return entrypoint, nil
		}
	}

	entrypoint := path.Join(rootDir, alias, "index.html")
	if exists(entrypoint) {
		return entrypoint, nil
	}

	return "", fmt.Errorf("entrypoint not found")
}

func loadEnv(root string, entrypoint string) (map[string]string, error) {
	if filepath.Ext(entrypoint) == ".html" {
		return make(map[string]string), nil
	}

	env := make(map[string]string)
	rootEnvPath := filepath.Join(root, ".env")
	if exists(rootEnvPath) {
		rootEnv, err := godotenv.Read(rootEnvPath)
		if err != nil {
			return nil, err
		}
		env = rootEnv
	}

	envPath := filepath.Join(filepath.Dir(entrypoint), ".env")
	if !exists(envPath) || envPath == rootEnvPath {
		return env, nil
	}

	dirEnv, err := godotenv.Read(envPath)
	if err != nil {
		return nil, err
	}

	for k, v := range dirEnv {
		env[k] = v
	}

	return env, nil
}

func main() {
	cmd := NewCmdRoot()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

type SerializedRequest struct {
	Url     string     `json:"url"`
	Method  string     `json:"method"`
	Headers [][]string `json:"headers"`
	Body    []byte     `json:"body,omitempty"`
}

func serializeRequest(req *http.Request) (SerializedRequest, error) {
	var res SerializedRequest

	url := req.URL
	url.Host = req.Host
	if req.TLS != nil {
		url.Scheme = "https"
	} else {
		url.Scheme = "http"
	}
	res.Url = url.String()

	res.Method = req.Method
	for k, v := range req.Header {
		res.Headers = append(res.Headers, []string{k, v[0]})
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return res, err
	}
	res.Body = body

	return res, nil
}

type SerializedResponse struct {
	Status  int        `json:"status"`
	Headers [][]string `json:"headers"`
	Body    []byte     `json:"body"`
}

func NewCmdRoot() *cobra.Command {
	cmd := &cobra.Command{
		Use: "smallweb",
	}

	cmd.AddCommand(NewServeCmd())
	cmd.InitDefaultCompletionCmd()

	return cmd
}

func NewServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:  "serve",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := os.MkdirAll(dataHome, 0755); err != nil {
				return err
			}

			// refresh sandbox code
			if err := os.WriteFile(sandboxPath, sandboxBytes, 0644); err != nil {
				return err
			}

			rootDir := args[0]
			if !exists(rootDir) {
				return fmt.Errorf("directory %s does not exist", rootDir)
			}

			port, err := cmd.Flags().GetInt("port")
			if err != nil {
				return err
			}

			server := http.Server{
				Addr:    fmt.Sprintf(":%d", port),
				Handler: NewHandler(rootDir),
			}

			fmt.Fprintln(os.Stderr, "Listening on", server.Addr)
			return server.ListenAndServe()
		},
	}
	cmd.Flags().IntP("port", "p", 4321, "Port to listen on")
	return cmd
}

func NewHandler(rootDir string) http.Handler {
	return &Handler{rootDir: rootDir}

}

type Handler struct {
	rootDir string
}

type CommandInput struct {
	Req        SerializedRequest `json:"req"`
	Entrypoint string            `json:"entrypoint"`
	Env        map[string]string `json:"env"`
	Output     string            `json:"output"`
}

type Log struct {
	Request    *SerializedRequest  `json:"request"`
	Response   *SerializedResponse `json:"response,omitempty"`
	Timestamp  string              `json:"timestamp,omitempty"`
	Entrypoint string              `json:"entrypoint,omitempty"`
	Duration   time.Duration       `json:"duration,omitempty"`
	Logs       []byte              `json:"logs"`
}

func writeLog(log Log, logPath string) error {
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(f)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(log)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	alias := strings.Split(host, ".")[0]

	entrypoint, err := inferEntrypoint(h.rootDir, alias)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if path.Base(entrypoint) == "index.html" {
		server := http.FileServer(http.Dir(path.Dir(entrypoint)))
		server.ServeHTTP(w, r)
		return
	}

	env, err := loadEnv(h.rootDir, entrypoint)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	req, err := serializeRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tempdir, err := os.MkdirTemp("", "smallweb")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tempdir)
	output := path.Join(tempdir, "response.json")

	input := CommandInput{
		Req:        req,
		Entrypoint: entrypoint,
		Env:        env,
		Output:     output,
	}

	deno, err := denoExecutable()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cmd := exec.Command(deno, "run", "-A", "--unstable-kv", sandboxPath)
	cmd.Dir = path.Dir(entrypoint)
	stdin := bytes.Buffer{}
	encoder := json.NewEncoder(&stdin)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(input); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cmd.Stdin = &stdin
	logPath := path.Join(h.rootDir, ".logs", alias+".jsonl")

	timestamp := time.Now()
	outputBytes, err := cmd.CombinedOutput()
	duration := time.Since(timestamp)
	if err != nil {
		writeLog(Log{
			Timestamp:  timestamp.Format(time.RFC3339),
			Entrypoint: entrypoint,
			Request:    &req,
			Duration:   duration,
			Logs:       outputBytes,
		}, logPath)
		http.Error(w, string(outputBytes), http.StatusInternalServerError)
		return
	}

	f, err := os.Open(output)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var res SerializedResponse
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&res); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeLog(Log{
		Timestamp:  timestamp.Format(time.RFC3339),
		Entrypoint: entrypoint,
		Duration:   duration,
		Request:    &req,
		Response:   &res,
		Logs:       outputBytes,
	}, logPath)

	for _, header := range res.Headers {
		w.Header().Set(header[0], header[1])
	}

	w.WriteHeader(res.Status)
	if res.Body != nil {
		w.Write(res.Body)
	}
}
