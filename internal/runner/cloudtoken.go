package runner

import (
	"context"
	"fmt"
	"io"

	"github.com/openfactory-org/openfactory/internal/cloudid"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
)

// Output formats of the cloud token helper.
const (
	// CloudTokenFormatToken prints only the token.
	CloudTokenFormatToken = "token"
	// CloudTokenFormatGCP prints the response of a GCP executable credential source.
	CloudTokenFormatGCP = "gcp"
)

// CloudToken is the cloud token helper. It asks the control plane for a cloud
// identity token and writes only the token to out. It reads the control plane
// URL and the session token from the session file, so the SDK that calls the
// helper never sees the session token. A ttlSeconds of 0 asks for the default
// life.
func CloudToken(ctx context.Context, stateDir, audience string, ttlSeconds int, out io.Writer) error {
	return CloudTokenAs(ctx, stateDir, audience, ttlSeconds, CloudTokenFormatToken, out)
}

// CloudTokenAs is CloudToken with an output format: CloudTokenFormatToken
// or CloudTokenFormatGCP.
func CloudTokenAs(ctx context.Context, stateDir, audience string, ttlSeconds int, format string, out io.Writer) error {
	if audience == "" {
		return fmt.Errorf("cloud token: audience is required")
	}
	if format == "" {
		format = CloudTokenFormatToken
	}
	if format != CloudTokenFormatToken && format != CloudTokenFormatGCP {
		return fmt.Errorf("cloud token: unknown format %q", format)
	}
	sf, err := loadSession(stateDir)
	if err != nil {
		return err
	}
	c, err := runnerclient.New(runnerclient.Options{BaseURL: sf.ControlURL, RunnerVersion: sf.RunnerVersion})
	if err != nil {
		return err
	}
	c.SetToken(sf.SessionToken)
	resp, err := c.CloudToken(ctx, audience, ttlSeconds)
	if err != nil {
		return fmt.Errorf("cloud token: %w", err)
	}
	if format == CloudTokenFormatGCP {
		b, err := cloudid.GCPExecutableResponse(resp.Token, resp.ExpiresAt)
		if err != nil {
			return err
		}
		_, err = out.Write(b)
		return err
	}
	_, err = fmt.Fprint(out, resp.Token)
	return err
}
