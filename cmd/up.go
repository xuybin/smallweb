package cmd

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "embed"

	"github.com/adrg/xdg"
	"github.com/gobwas/glob"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/pomdtr/smallweb/app"
	"github.com/pomdtr/smallweb/database"
	"github.com/pomdtr/smallweb/docs"
	"github.com/pomdtr/smallweb/editor"

	"github.com/pomdtr/smallweb/term"
	"github.com/pomdtr/smallweb/utils"
	"github.com/pomdtr/smallweb/worker"
	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/webdav"
	"golang.org/x/oauth2"
)

type AuthMiddleware struct {
	db *sql.DB
}

func (me *AuthMiddleware) CreateSession(email string, domain string) (string, error) {
	sessionID, err := gonanoid.New()
	if err != nil {
		return "", fmt.Errorf("failed to generate session ID: %w", err)
	}

	session := database.Session{
		ID:        sessionID,
		Email:     email,
		Domain:    domain,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(14 * 24 * time.Hour),
	}

	if err := database.InsertSession(me.db, &session); err != nil {
		return "", fmt.Errorf("failed to insert session: %w", err)
	}

	return sessionID, nil
}

func (me *AuthMiddleware) DeleteSession(sessionID string) error {
	if err := database.DeleteSession(me.db, sessionID); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	return nil
}

func (me *AuthMiddleware) GetSession(sessionID string, domain string) (database.Session, error) {
	session, err := database.GetSession(me.db, sessionID)
	if err != nil {
		return database.Session{}, fmt.Errorf("failed to get session: %w", err)
	}

	if session.Domain != domain {
		return database.Session{}, fmt.Errorf("session not found")
	}

	return *session, nil
}

func (me *AuthMiddleware) ExtendSession(sessionID string, expiresAt time.Time) error {
	session, err := database.GetSession(me.db, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}

	session.ExpiresAt = expiresAt
	if err := database.UpdateSession(me.db, session); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	return nil
}

func (me *AuthMiddleware) Wrap(next http.Handler, email string) http.Handler {
	sessionCookieName := "smallweb-session"
	oauthCookieName := "smallweb-oauth-store"
	type oauthStore struct {
		State    string `json:"state"`
		Redirect string `json:"redirect"`
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, _, ok := r.BasicAuth()
		if ok {
			public, secret, err := parseToken(username)
			if err != nil {
				w.Header().Add("WWW-Authenticate", `Basic realm="smallweb"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			token, err := database.GetToken(me.db, public)
			if err != nil {
				w.Header().Add("WWW-Authenticate", `Basic realm="smallweb"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			if bcrypt.CompareHashAndPassword([]byte(token.Hash), []byte(secret)) != nil {
				w.Header().Add("WWW-Authenticate", `Basic realm="smallweb"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
			return
		}

		authorization := r.Header.Get("Authorization")
		if strings.HasPrefix(authorization, "Bearer ") {
			public, secret, err := parseToken(strings.TrimPrefix(authorization, "Bearer "))
			if err != nil {
				w.Header().Add("WWW-Authenticate", `Bearer realm="smallweb"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			t, err := database.GetToken(me.db, public)
			if err != nil {
				w.Header().Add("WWW-Authenticate", `Bearer realm="smallweb"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			if bcrypt.CompareHashAndPassword([]byte(t.Hash), []byte(secret)) != nil {
				w.Header().Add("WWW-Authenticate", `Bearer realm="smallweb"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
			return
		}

		if email == "" {
			w.Header().Add("WWW-Authenticate", `Basic realm="smallweb"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		oauth2Config := oauth2.Config{
			ClientID: fmt.Sprintf("https://%s/", r.Host),
			Endpoint: oauth2.Endpoint{
				AuthURL:   "https://lastlogin.net/auth",
				TokenURL:  "https://lastlogin.net/token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
			Scopes:      []string{"email"},
			RedirectURL: fmt.Sprintf("https://%s/_auth/callback", r.Host),
		}

		if r.URL.Path == "/_auth/login" {
			query := r.URL.Query()
			state, err := generateBase62String(16)
			if err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			store := oauthStore{
				State:    state,
				Redirect: query.Get("redirect"),
			}

			value, err := json.Marshal(store)
			if err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}

			http.SetCookie(w, &http.Cookie{
				Name:     oauthCookieName,
				Value:    url.QueryEscape(string(value)),
				Expires:  time.Now().Add(5 * time.Minute),
				Path:     "/",
				SameSite: http.SameSiteLaxMode,
				HttpOnly: true,
				Secure:   true,
			})

			url := oauth2Config.AuthCodeURL(state)
			http.Redirect(w, r, url, http.StatusSeeOther)
			return
		}

		if r.URL.Path == "/_auth/callback" {
			query := r.URL.Query()
			oauthCookie, err := r.Cookie(oauthCookieName)
			if err != nil {
				log.Printf("failed to get oauth cookie: %v", err)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			var oauthStore oauthStore
			value, err := url.QueryUnescape(oauthCookie.Value)
			if err != nil {
				log.Printf("failed to unescape oauth cookie: %v", err)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			if err := json.Unmarshal([]byte(value), &oauthStore); err != nil {
				log.Printf("failed to unmarshal oauth cookie: %v", err)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			if query.Get("state") != oauthStore.State {
				log.Printf("state mismatch: %s != %s", query.Get("state"), oauthStore.State)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			code := query.Get("code")
			if code == "" {
				log.Printf("code not found")
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}

			token, err := oauth2Config.Exchange(r.Context(), code)
			if err != nil {
				log.Printf("failed to exchange code: %v", err)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			req, err := http.NewRequest("GET", "https://lastlogin.net/userinfo", nil)
			if err != nil {
				log.Printf("failed to create userinfo request: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Printf("failed to execute userinfo request: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				log.Printf("userinfo request failed: %s", resp.Status)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			var userinfo struct {
				Email string `json:"email"`
			}

			if err := json.NewDecoder(resp.Body).Decode(&userinfo); err != nil {
				log.Printf("failed to decode userinfo: %v", err)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			sessionID, err := me.CreateSession(userinfo.Email, r.Host)
			if err != nil {
				log.Printf("failed to create session: %v", err)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// delete oauth cookie
			http.SetCookie(w, &http.Cookie{
				Name:     oauthCookieName,
				Expires:  time.Now().Add(-1 * time.Hour),
				Path:     "/",
				SameSite: http.SameSiteLaxMode,
				HttpOnly: true,
				Secure:   true,
			})

			// set session cookie
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookieName,
				Value:    sessionID,
				Expires:  time.Now().Add(14 * 24 * time.Hour),
				SameSite: http.SameSiteLaxMode,
				HttpOnly: true,
				Secure:   true,
				Path:     "/",
			})

			http.Redirect(w, r, oauthStore.Redirect, http.StatusSeeOther)
			return
		}

		if r.URL.Path == "/_auth/logout" {
			cookie, err := r.Cookie(sessionCookieName)
			if err != nil {
				log.Printf("failed to get session cookie: %v", err)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			if err := me.DeleteSession(cookie.Value); err != nil {
				log.Printf("failed to delete session: %v", err)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookieName,
				Expires:  time.Now().Add(-1 * time.Hour),
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteLaxMode,
				Path:     "/",
			})

			redirect := r.URL.Query().Get("redirect")
			if redirect == "" {
				redirect = fmt.Sprintf("https://%s/", r.Host)
			}

			http.Redirect(w, r, redirect, http.StatusSeeOther)
			return
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Redirect(w, r, fmt.Sprintf("/_auth/login?redirect=%s", r.URL.Path), http.StatusSeeOther)
			return
		}

		session, err := me.GetSession(cookie.Value, r.Host)
		if err != nil {
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookieName,
				Expires:  time.Now().Add(-1 * time.Hour),
				SameSite: http.SameSiteLaxMode,
				HttpOnly: true,
				Secure:   true,
			})

			http.Redirect(w, r, fmt.Sprintf("/_auth/login?redirect=%s", r.URL.Path), http.StatusSeeOther)
			return
		}

		if time.Now().After(session.ExpiresAt) {
			if err := me.DeleteSession(cookie.Value); err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookieName,
				Expires:  time.Now().Add(-1 * time.Hour),
				SameSite: http.SameSiteLaxMode,
				HttpOnly: true,
				Secure:   true,
			})

			http.Redirect(w, r, fmt.Sprintf("/_auth/login?redirect=%s", r.URL.Path), http.StatusSeeOther)
			return
		}

		if session.Email != email {
			log.Printf("email mismatch: %s != %s", session.Email, email)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// if session is near expiration, extend it
		if time.Now().Add(7 * 24 * time.Hour).After(session.ExpiresAt) {
			if err := me.ExtendSession(cookie.Value, time.Now().Add(14*24*time.Hour)); err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			return
		}

		next.ServeHTTP(w, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}

	return nil, nil, fmt.Errorf("Hijack not supported")
}

func loggingMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rw := &responseWriter{w, http.StatusOK}
		next.ServeHTTP(rw, r)

		duration := time.Since(start)

		logger.LogAttrs(context.Background(), slog.LevelInfo, "Request completed",
			slog.String("method", r.Method),
			slog.String("host", r.Host),
			slog.String("path", r.URL.Path),
			slog.Int("status", rw.statusCode),
			slog.Duration("duration", duration),
		)
	})
}

func NewCmdUp(db *sql.DB) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "up",
		Short:   "Start the smallweb evaluation server",
		GroupID: CoreGroupID,
		Aliases: []string{"serve"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			rootDir := utils.ExpandTilde(k.String("dir"))
			domain := k.String("domain")
			port := k.Int("port")
			cert := k.String("cert")
			key := k.String("key")

			if port == 0 {
				if cert != "" || key != "" {
					port = 443
				} else {
					port = 7777
				}
			}

			webdavHandler := &webdav.Handler{
				FileSystem: webdav.Dir(rootDir),
				LockSystem: webdav.NewMemLS(),
			}

			editorHandler, err := editor.NewHandler(rootDir)
			if err != nil {
				return fmt.Errorf("failed to create editor handler: %w", err)
			}

			sessionDBPath := filepath.Join(xdg.DataHome, "smallweb", "sessions.json")
			if err := os.MkdirAll(filepath.Dir(sessionDBPath), 0755); err != nil {
				return fmt.Errorf("failed to create session database directory: %w", err)
			}

			cliHandler := term.NewHandler(k.String("shell"))
			cliHandler.Dir = rootDir
			docsHandler, err := docs.NewHandler()
			if err != nil {
				return fmt.Errorf("failed to create docs handler: %w", err)
			}

			authMiddleware := AuthMiddleware{db}
			addr := fmt.Sprintf("%s:%d", k.String("host"), port)
			server := http.Server{
				Addr: addr,
				Handler: loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Host == domain {
						target := r.URL
						target.Scheme = "https"
						target.Host = "www." + domain
						http.Redirect(w, r, target.String(), http.StatusTemporaryRedirect)
						return
					}

					appname := strings.TrimSuffix(r.Host, fmt.Sprintf(".%s", domain))
					a, err := app.LoadApp(filepath.Join(rootDir, appname), k.String("domain"))
					if err != nil {
						w.WriteHeader(http.StatusNotFound)
						return
					}

					var handler http.Handler
					switch a.Entrypoint() {
					case "smallweb:webdav":
						handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Access-Control-Allow-Origin", "*")
							w.Header().Set("Access-Control-Allow-Methods", "*")
							w.Header().Set("Access-Control-Allow-Headers", "*")
							if r.Method == "OPTIONS" {
								return
							}

							webdavHandler.ServeHTTP(w, r)
						})
					case "smallweb:cli":
						handler = cliHandler
					case "smallweb:docs":
						handler = docsHandler
					case "smallweb:static":
						handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Access-Control-Allow-Origin", "*")
							w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
							w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
							if r.Method == "OPTIONS" {
								return
							}
							http.FileServer(http.Dir(a.Root())).ServeHTTP(w, r)
						})
					case "smallweb:editor":
						handler = editorHandler
					default:
						wk := worker.NewWorker(a, k.StringMap("env"))
						if err := wk.StartServer(); err != nil {
							http.Error(w, err.Error(), http.StatusInternalServerError)
							return
						}
						defer wk.StopServer()
						handler = wk
					}

					isPrivateRoute := a.Config.Private
					for _, publicRoute := range a.Config.PublicRoutes {
						glob := glob.MustCompile(publicRoute)
						if glob.Match(r.URL.Path) {
							isPrivateRoute = false
						}
					}

					for _, privateRoute := range a.Config.PrivateRoutes {
						glob := glob.MustCompile(privateRoute)
						if glob.Match(r.URL.Path) {
							isPrivateRoute = true
						}
					}

					if isPrivateRoute || strings.HasPrefix(r.URL.Path, "/_auth") {
						handler = authMiddleware.Wrap(handler, k.String("email"))
					}

					handler.ServeHTTP(w, r)
				}), logger),
			}

			parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
			c := cron.New(cron.WithParser(parser))
			c.AddFunc("* * * * *", func() {
				rootDir := utils.ExpandTilde(k.String("dir"))
				rounded := time.Now().Truncate(time.Minute)
				apps, err := app.ListApps(rootDir)
				if err != nil {
					fmt.Println(err)
				}

				for _, name := range apps {
					a, err := app.LoadApp(filepath.Join(rootDir, name), k.String("domain"))
					if err != nil {
						fmt.Println(err)
						continue
					}

					for _, job := range a.Config.Crons {
						sched, err := parser.Parse(job.Schedule)
						if err != nil {
							fmt.Println(err)
							continue
						}

						if sched.Next(rounded.Add(-1*time.Second)) != rounded {
							continue
						}

						wk := worker.NewWorker(a, k.StringMap("env"))

						command, err := wk.Command(job.Args...)
						if err != nil {
							fmt.Println(err)
							continue
						}

						if err := command.Run(); err != nil {
							fmt.Println(err)
						}
					}

				}
			})

			go c.Start()

			if cert != "" || key != "" {
				if cert == "" {
					return fmt.Errorf("TLS certificate file is required")
				}

				if key == "" {
					return fmt.Errorf("TLS key file is required")
				}

				cmd.Printf("Serving %s from %s on %s\n", k.String("domain"), k.String("dir"), addr)
				return server.ListenAndServeTLS(utils.ExpandTilde(cert), utils.ExpandTilde(key))
			}

			cmd.Printf("Serving *.%s from %s on %s\n", k.String("domain"), k.String("dir"), addr)
			return server.ListenAndServe()
		},
	}

	return cmd
}
