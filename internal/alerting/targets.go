package alerting

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// errBlockedAddress is what the dial guard answers for an address a tenant's
// webhook may not reach. It deliberately names no address.
var errBlockedAddress = errors.New("the address is private or internal")

// blockedPrefixes are the addresses a webhook that a tenant (not the platform
// operator) configured must never be sent to: the server makes the request from
// inside its own network, so a private address would turn "send me a message"
// into "probe whatever this host can reach". It carries the ranges the
// management API-call guard refuses plus the documentation ranges, which are
// never a real receiver.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// addrBlocked reports whether addr is one of blockedPrefixes. An IPv4 address
// written as IPv4-mapped IPv6 is judged as the IPv4 address it is.
func addrBlocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// publicOnlyControl is a net.Dialer Control hook that refuses to connect to a
// blocked address. It runs after name resolution and before every connection
// attempt, so a host name that resolves (or later re-resolves) to a private
// address is stopped as well as a literal one; checking the URL alone would not
// survive DNS rebinding.
func publicOnlyControl(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil || addrBlocked(addrPort.Addr()) {
		return errBlockedAddress
	}
	return nil
}

// internalHostSuffixes are host names that only mean something inside a
// network. They are refused up front so the operator gets an explanation when
// saving, instead of a connection error when the first alert is due.
var internalHostSuffixes = []string{".localhost", ".local", ".internal", ".lan", ".intranet", ".home.arpa"}

func internalHostname(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range internalHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// parseTarget parses a webhook address and rejects the shapes no receiver uses.
func parseTarget(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("is required")
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f {
			return nil, errors.New("must not contain spaces or control characters")
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return nil, errors.New("must be a full URL such as https://example.com/hook")
	}
	if parsed.User != nil {
		return nil, errors.New("must not contain a user name or password")
	}
	if port := parsed.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, errors.New("has an invalid port")
		}
	}
	return parsed, nil
}

// ValidateWebhookURL checks the address of a generic webhook. allowPrivate says
// whether the platform operator is the one configuring it: only then may it be
// plain http or point into the local network (an alert relay on the same
// machine is the usual reason). Everyone else gets https and public addresses
// only, here for what can be seen in the URL and again at connection time for
// what a host name resolves to (see publicOnlyControl).
func ValidateWebhookURL(raw string, allowPrivate bool) error {
	parsed, err := parseTarget(raw)
	if err != nil {
		return err
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !allowPrivate {
			return errors.New("must use https")
		}
	default:
		return errors.New("must be an http or https URL")
	}
	if allowPrivate {
		return nil
	}
	host := parsed.Hostname()
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if addrBlocked(addr) {
			return errors.New("must not point to a private or internal address")
		}
		return nil
	}
	if internalHostname(host) {
		return errors.New("must not point to a private or internal address")
	}
	return nil
}

// feishuHosts are the Feishu / Lark open platform hosts custom bots live on.
var feishuHosts = map[string]struct{}{
	"open.feishu.cn":     {},
	"open.larksuite.com": {},
}

const feishuHookPrefix = "/open-apis/bot/v2/hook/"

var feishuTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// ValidateFeishuURL checks the address of a Feishu or Lark custom bot. The host
// is fixed, which is also why this channel needs no private-address guard: a
// tenant cannot aim it anywhere else.
func ValidateFeishuURL(raw string) error {
	parsed, err := parseTarget(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" {
		return errors.New("must use https")
	}
	if _, ok := feishuHosts[strings.ToLower(parsed.Hostname())]; !ok {
		return fmt.Errorf("must be a Feishu or Lark custom bot address (https://open.feishu.cn%s...)", feishuHookPrefix)
	}
	token, ok := strings.CutPrefix(parsed.Path, feishuHookPrefix)
	if !ok || !feishuTokenPattern.MatchString(token) || parsed.RawQuery != "" {
		return fmt.Errorf("must look like https://open.feishu.cn%s<token>", feishuHookPrefix)
	}
	return nil
}

var (
	telegramTokenPattern = regexp.MustCompile(`^[0-9]{5,16}:[A-Za-z0-9_-]{30,64}$`)
	telegramChatPattern  = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z][A-Za-z0-9_]{4,31})$`)
)

// ValidateTelegramToken checks the shape of a bot token ("<id>:<secret>"). The
// shape matters beyond tidiness: the token is placed in the request path, so a
// value that could add path segments or a query must never get that far.
func ValidateTelegramToken(token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("is required")
	}
	if !telegramTokenPattern.MatchString(token) {
		return errors.New("must look like 123456789:AA... (the token BotFather gave you)")
	}
	return nil
}

// ValidateTelegramChatID checks a chat id: a number (negative for groups and
// channels) or an @channelusername.
func ValidateTelegramChatID(chatID string) error {
	if strings.TrimSpace(chatID) == "" {
		return errors.New("is required")
	}
	if !telegramChatPattern.MatchString(chatID) {
		return errors.New("must be a chat id such as -1001234567890, or @channelname")
	}
	return nil
}
