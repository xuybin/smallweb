package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/pomdtr/smallweb/server/frontend"
	"github.com/pomdtr/smallweb/server/storage"
	"golang.org/x/crypto/ssh"
)

type contextKey struct {
	name string
}

var (
	ContextKeyEmail = &contextKey{"email"}
)

type RootHandler struct {
	db        *storage.DB
	forwarder *Forwarder
}

func NewRootHandler(db *storage.DB, forwarder *Forwarder) *RootHandler {
	return &RootHandler{
		db:        db,
		forwarder: forwarder,
	}
}

func (me *RootHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		email, ok := r.Context().Value(ContextKeyEmail).(string)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		}

		user, err := me.db.GetUserWithEmail(email)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if ok, payload, err := me.forwarder.SendRequest(user.Name, "list-apps", nil); err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		} else if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		} else {
			var resp ListAppsResponse
			if err := ssh.Unmarshal(payload, &resp); err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			w.WriteHeader(http.StatusOK)
			// TODO: write a template for this
			var links []frontend.Link
			for _, app := range resp.Apps {
				links = append(links, frontend.Link{
					Href: fmt.Sprintf("https://%s-%s.%s", app, user.Name, r.Host),
					Text: app,
				})
			}

			frontend.Layout("Smallweb", frontend.LinkList(links)).Render(context.Background(), w)
			return
		}
	})

	mux.ServeHTTP(w, r)
}
