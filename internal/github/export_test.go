package github

import "net/http"

// APIVersion exposes the API version header value to the external tests.
const APIVersion = apiVersion

// AuthBaseTransport returns the transport that NewClients put under client's
// authentication wrapper, or nil if client is not one of its API clients.
func AuthBaseTransport(client *http.Client) http.RoundTripper {
	auth, ok := client.Transport.(*authTransport)
	if !ok {
		return nil
	}

	return auth.base
}
