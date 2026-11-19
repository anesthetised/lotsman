package web

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/ratelimit"
	"github.com/anesthetised/lotsman/internal/store"
)

// A user's link is <public_url>/u#<token>. The token is in the fragment,
// which browsers never send, so it stays out of proxy access logs; the page
// reads it and sends it on every call as "Authorization: Bearer <token>".
// A header rather than a cookie: the link is meant to be bookmarked and
// needs no sign-in step, no other site can make a browser send the header,
// so there is nothing to forge, and it stays apart from operators' session
// cookies. The panel never forwards it.

const (
	// linkTTL is how long a resolved link is trusted without asking the
	// daemon again: a revoked link keeps working for at most this long.
	linkTTL = 30 * time.Second
	// maxCachedLinks bounds the cache; past it, links are resolved anew.
	maxCachedLinks = 1024
	// A client may fail linkFailBurst times at once and then once every
	// linkFailEvery. A token has 256 bits, so this protects the daemon from
	// load, not the tokens from guessing.
	linkFailBurst = 10
	linkFailEvery = 6 * time.Second
	// resolveTimeout bounds one question to the daemon.
	resolveTimeout = 5 * time.Second
)

// Resolver turns a link's token into its user: the daemon's
// UserSelfService.ResolveLink, or a fake in tests. A token that names no
// one fails with connect.CodeNotFound.
type Resolver interface {
	Resolve(ctx context.Context, token string) (string, error)
}

// DaemonResolver resolves links over the daemon's control socket.
func DaemonResolver(daemon http.RoundTripper) Resolver {
	t := connecthttp.NewTransport(&http.Client{Transport: daemon}, "http://lotsman")
	return daemonResolver{lotsmanv1connect.NewUserSelfServiceClient(connect.NewClient(t))}
}

type daemonResolver struct {
	client lotsmanv1connect.UserSelfServiceClient
}

func (d daemonResolver) Resolve(ctx context.Context, token string) (string, error) {
	res, err := d.client.ResolveLink(ctx, &lotsmanv1.ResolveLinkRequest{Token: token})
	return res.GetUser(), err
}

// Links signs users in by their links. It keeps resolved links for linkTTL,
// keyed by the token's hash, and refuses a client address that failed too
// often. Neither its errors nor anything it logs contain a token.
type Links struct {
	resolver Resolver
	now      func() time.Time
	failures *ratelimit.Limiter[netip.Prefix]

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cachedLink
}

type cachedLink struct {
	user    string
	expires time.Time
}

func NewLinks(r Resolver) *Links {
	return &Links{
		resolver: r, now: time.Now, failures: ratelimit.New[netip.Prefix](linkFailEvery, linkFailBurst),
		cache: map[[sha256.Size]byte]cachedLink{},
	}
}

// WithLinks is auth with users signed in by their links.
func WithLinks(auth Authenticator, links *Links) Authenticator { return linkAuth{auth, links} }

type linkAuth struct {
	Authenticator
	links *Links
}

func (a linkAuth) User(r *http.Request) (string, error) { return a.links.User(r) }

var (
	errNoLink      = connect.NewError(connect.CodeUnauthenticated, "no link")
	errUnknownLink = connect.NewError(connect.CodeUnauthenticated, "unknown or revoked link")
	errTooMany     = connect.NewError(connect.CodeResourceExhausted, "too many unknown links from this address; try again later")
)

// User returns the user whose link r carries.
func (l *Links) User(r *http.Request) (string, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", errNoLink
	}
	key := sha256.Sum256([]byte(token))
	now := l.now()
	l.mu.Lock()
	c, ok := l.cache[key]
	l.mu.Unlock()
	if ok && now.Before(c.expires) {
		return c.user, nil
	}

	client := clientPrefix(ClientAddr(r))
	if l.failures.Exhausted(client, now) {
		return "", errTooMany
	}
	var user string
	var err error = errUnknownLink
	if store.ValidToken(token) {
		ctx, cancel := context.WithTimeout(r.Context(), resolveTimeout)
		user, err = l.resolver.Resolve(ctx, token)
		cancel()
	}
	switch {
	case err == nil && user != "":
		l.remember(key, user, now)
		return user, nil
	case err == nil, err == errUnknownLink, connect.CodeOf(err) == connect.CodeNotFound:
		l.forget(key)
		l.failures.Allow(client, now)
		return "", errUnknownLink
	}
	// The daemon did not answer; that is no failure of the client's.
	return "", connect.NewError(connect.CodeUnavailable, "the gateway daemon is not reachable")
}

func (l *Links) remember(key [sha256.Size]byte, user string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.cache) >= maxCachedLinks {
		for k, c := range l.cache {
			if !now.Before(c.expires) {
				delete(l.cache, k)
			}
		}
		if len(l.cache) >= maxCachedLinks {
			return
		}
	}
	l.cache[key] = cachedLink{user: user, expires: now.Add(linkTTL)}
}

func (l *Links) forget(key [sha256.Size]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.cache, key)
}
