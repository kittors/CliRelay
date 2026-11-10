// Package antigravity provides OAuth2 authentication functionality for the Antigravity provider.
package antigravity

// OAuth client credentials and configuration
const (
	CallbackPort = 51121
)

// Scopes defines the OAuth scopes required for Antigravity authentication
var Scopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// OAuth2 endpoints for Google authentication
const (
	TokenEndpoint    = "https://oauth2.googleapis.com/token"
	AuthEndpoint     = "https://accounts.google.com/o/oauth2/v2/auth"
	UserInfoEndpoint = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"
)

// Antigravity API configuration
const (
	APIEndpoint = "https://cloudcode-pa.googleapis.com"
	APIVersion  = "v1internal"
	// APIUserAgent is the Google OAuth client identity, shared with gemini-cli.
	// It applies to the token and userinfo endpoints, not to CloudCode calls —
	// those advertise the Antigravity client itself via ClientUserAgent.
	APIUserAgent   = "google-api-nodejs-client/9.15.1"
	APIClient      = "google-cloud-sdk vscode_cloudshelleditor/0.1"
	ClientMetadata = `{"ideType":"IDE_UNSPECIFIED","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}`
)

// ClientVersion is the Antigravity Hub version advertised to CloudCode.
//
// The upstream gates its answer on this: an outdated version is served a
// reduced model set and a coarser quota view. Hub 2.22.0 and later are served
// Gemini 4 Argon, 2.9.1 is not, and the vscode/1.X.X (Antigravity/x) identity
// this code advertised before is not served it at any version number.
//
// Keep this in step with the shipping Hub release when models start coming back
// missing or with a flattened quota view.
const ClientVersion = "2.26.0"

// ClientUserAgent is the header the Antigravity Hub's language server sends on
// its own CloudCode calls.
const ClientUserAgent = "antigravity/hub/" + ClientVersion + " darwin/arm64"
