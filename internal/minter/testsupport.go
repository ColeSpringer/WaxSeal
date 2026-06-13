package minter

import "context"

// InjectSessionForTest installs sess as generation 1 for the tenant selected by
// apiKey and returns that tenant's Minter.
//
// Tests in dependent packages use it to exercise live-session handlers without
// launching Chromium. Production code must not call it.
func (t *Tenants) InjectSessionForTest(ctx context.Context, apiKey string, sess minterSession) (*Minter, error) {
	m, _, err := t.Minter(apiKey)
	if err != nil {
		return nil, err
	}
	m.launch = func(context.Context) (minterSession, error) { return sess, nil }
	if err := m.Warm(ctx); err != nil {
		return nil, err
	}
	return m, nil
}
