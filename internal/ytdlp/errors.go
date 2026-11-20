package ytdlp

import (
	"errors"
	"strings"
)

// Kind classifies why a yt-dlp operation failed, so clients can tell the user
// what actually went wrong (YouTube said no, the server can't reach YouTube,
// yt-dlp is too old…) instead of one opaque failure.
type Kind string

const (
	KindBusy        Kind = "ytdlp_busy"
	KindBadTarget   Kind = "bad_target"
	KindUnavailable Kind = "video_unavailable" // private, removed, geo-blocked, members-only
	KindAgeRestrict Kind = "age_restricted"
	KindBlocked     Kind = "youtube_blocked" // "confirm you're not a bot", sign-in wall
	KindRateLimited Kind = "youtube_rate_limited"
	KindNetwork     Kind = "server_network" // the server can't reach YouTube
	KindTimeout     Kind = "ytdlp_timeout"
	KindJSRuntime   Kind = "ytdlp_js_runtime" // no usable JavaScript runtime
	KindOutdated    Kind = "ytdlp_outdated"   // extractor broke / no formats
	KindTooLong     Kind = "video_too_long"
	KindTooLarge    Kind = "file_too_large"
	KindNothing     Kind = "nothing_downloaded"
	KindFailed      Kind = "ytdlp_failed"
)

// Error is a classified failure. Msg is safe to show to end users; Detail is
// the technical cause (yt-dlp's last stderr line) meant for logs and bug reports.
type Error struct {
	Kind   Kind
	Msg    string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Msg + " (" + e.Detail + ")"
	}
	return e.Msg
}

// AsError returns the *Error inside err, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// classify maps yt-dlp's stderr to a Kind and a user-facing message.
func classify(stderr string) *Error {
	detail := lastLine(stderr)
	low := strings.ToLower(stderr)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(low, s) {
				return true
			}
		}
		return false
	}
	mk := func(k Kind, msg string) *Error { return &Error{Kind: k, Msg: msg, Detail: detail} }

	switch {
	case has("confirm your age", "age-restricted", "age restricted", "inappropriate for some users"):
		return mk(KindAgeRestrict, "This video is age-restricted, and the server can't sign in to YouTube to watch it.")
	case has("not a bot", "sign in to confirm", "login required", "please sign in"):
		return mk(KindBlocked, "YouTube is asking this server to prove it isn't a bot. Try again later, or route yt-dlp through a different network (DIAPASON_YTDLP_PROXY).")
	case has("http error 429", "too many requests", "rate-limit", "rate limit"):
		return mk(KindRateLimited, "YouTube is rate-limiting this server. Wait a few minutes and try again.")
	case has("private video", "video unavailable", "video is unavailable", "this video is not available", "has been removed", "no longer available",
		"not available in your country", "blocked it in your country", "copyright", "members-only", "join this channel", "terminated"):
		return mk(KindUnavailable, "YouTube says this video isn't available (private, removed or blocked in the server's country).")
	case has("javascript runtime", "js runtime", "no supported javascript", "challenge solving failed", "n challenge"):
		return mk(KindJSRuntime, "yt-dlp needs a JavaScript runtime (deno or node) on the server to read YouTube and none was found.")
	case has("requested format is not available", "no video formats", "unable to extract", "unsupported url", "sig function", "nsig"):
		return mk(KindOutdated, "yt-dlp couldn't read YouTube's response — it is probably out of date. It updates itself daily; you can also update it manually.")
	case has("unable to download", "temporary failure in name resolution", "name or service not known", "connection refused",
		"connection reset", "network is unreachable", "no route to host", "timed out", "getaddrinfo", "ssl", "tlsv1", "proxy"):
		return mk(KindNetwork, "The server couldn't reach YouTube. Check its internet connection, DNS, or the configured yt-dlp proxy.")
	case has("larger than max-filesize", "file is larger"):
		return mk(KindTooLarge, "That video's audio is larger than this server allows.")
	}
	return mk(KindFailed, "yt-dlp failed on the server.")
}
