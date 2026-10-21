package httpapi

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zsoftly/zs-core/internal/kbplugin"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
)

// getPluginJar serves a pinned plugin jar from the plugin mirror, for KPM.
// It is not a brand route: it needs the deployment's mirror secret as a
// bearer credential, or the jar's own token in the token query parameter,
// because KPM sends no headers. The sha256 query parameter must match the
// caller's expected pin and is bound into the token. The mirror checks and
// snapshots the jar before serving. Without a mirror directory and a
// secret the route answers 404.
func (s *server) getPluginJar(w http.ResponseWriter, r *http.Request) {
	if s.pluginMirror == nil || len(s.pluginMirrorSecret) == 0 {
		s.writeError(w, r, zserrors.New(zserrors.CodeNotFound, ""))
		return
	}
	name := r.PathValue("name")
	version, ok := strings.CutSuffix(r.PathValue("file"), ".jar")
	if !ok || version == "" {
		s.writeError(w, r, zserrors.New(zserrors.CodeNotFound, ""))
		return
	}
	if !s.mirrorAuthorized(r, name, version) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="zs-core plugin mirror"`)
		s.writeError(w, r, zserrors.New(zserrors.CodeUnauthenticated, "The plugin mirror needs its secret or the jar's token."))
		return
	}
	q := r.URL.Query()
	pin, pinned := s.pluginMirror.Manifest.PinnedPlugin(name)
	if len(q["sha256"]) != 1 || !pinned || subtle.ConstantTimeCompare([]byte(q.Get("sha256")), []byte(pin.Sha256)) != 1 {
		s.writeError(w, r, zserrors.New(zserrors.CodeNotFound, ""))
		return
	}
	f, info, err := s.pluginMirror.Open(name, version)
	switch {
	case errors.Is(err, kbplugin.ErrNotInMirror):
		s.writeError(w, r, zserrors.New(zserrors.CodeNotFound, ""))
		return
	case errors.Is(err, kbplugin.ErrDigestMismatch), errors.Is(err, kbplugin.ErrManifestSignature):
		// A jar that does not match its pin, or a manifest Core did not
		// sign, is refused and recorded in the log for the operator.
		s.log.LogAttrs(r.Context(), slog.LevelError, "plugin mirror refused a jar",
			slog.String("event", "plugin.mirror.refused"), slog.String("plugin", name), slog.String("version", version), slog.String("reason", err.Error()))
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeInternal, "", err))
		return
	case err != nil:
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeInternal, "", err))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/java-archive")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (s *server) mirrorAuthorized(r *http.Request, name, version string) bool {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return subtle.ConstantTimeCompare([]byte(bearer), s.pluginMirrorSecret) == 1
	}
	return kbplugin.CheckToken(s.pluginMirrorSecret, name, version, r.URL.Query().Get("sha256"), r.URL.Query().Get("token"))
}
