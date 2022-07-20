/*
 * Copyright © 2015-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * @author		Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @copyright 	2015-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @license 	Apache-2.0
 *
 */

package rfc7523

import (
	"context"
	"time"

	"github.com/ory/fosite/handler/oauth2"

	"gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"

	"github.com/ory/fosite"
	"github.com/ory/x/errorsx"
)

const grantTypeJWTBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer"

type Handler struct {
	Storage                  RFC7523KeyStorage
	ScopeStrategy            fosite.ScopeStrategy
	AudienceMatchingStrategy fosite.AudienceMatchingStrategy

	// TokenURL is the the URL of the Authorization Server's Token Endpoint.
	TokenURL string
	// SkipClientAuth indicates, if client authentication can be skipped.
	SkipClientAuth bool
	// JWTIDOptional indicates, if jti (JWT ID) claim required or not.
	JWTIDOptional bool
	// JWTIssuedDateOptional indicates, if "iat" (issued at) claim required or not.
	JWTIssuedDateOptional bool
	// JWTMaxDuration sets the maximum time after token issued date (if present), during which the token is
	// considered valid. If "iat" claim is not present, then current time will be used as issued date.
	JWTMaxDuration time.Duration

	*oauth2.HandleHelper
}

// HandleTokenEndpointRequest implements https://tools.ietf.org/html/rfc6749#section-4.1.3 (everything) and
// https://tools.ietf.org/html/rfc7523#section-2.1 (everything)
func (c *Handler) HandleTokenEndpointRequest(ctx context.Context, request fosite.AccessRequester) error {
	if err := c.CheckRequest(request); err != nil {
		return err
	}

	assertion := request.GetRequestForm().Get("assertion")
	if assertion == "" {
		return errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("The assertion request parameter must be set when using grant_type of '%s'.", grantTypeJWTBearer))
	}

	token, err := jwt.ParseSigned(assertion)
	if err != nil {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("Unable to parse JSON Web Token passed in \"assertion\" request parameter.").
			WithWrap(err).WithDebug(err.Error()),
		)
	}

	// Check fo required claims in token, so we can later find public key based on them.
	if err := c.validateTokenPreRequisites(token); err != nil {
		return err
	}

	key, err := c.findPublicKeyForToken(ctx, token)
	if err != nil {
		return err
	}

	claims := jwt.Claims{}
	if err := token.Claims(key, &claims); err != nil {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("Unable to verify the integrity of the 'assertion' value.").
			WithWrap(err).WithDebug(err.Error()),
		)
	}

	if err := c.validateTokenClaims(ctx, claims, key); err != nil {
		return err
	}

	scopes, err := c.Storage.GetPublicKeyScopes(ctx, claims.Issuer, claims.Subject, key.KeyID)
	if err != nil {
		return errorsx.WithStack(fosite.ErrServerError.WithWrap(err).WithDebug(err.Error()))
	}

	for _, scope := range request.GetRequestedScopes() {
		if !c.ScopeStrategy(scopes, scope) {
			return errorsx.WithStack(fosite.ErrInvalidScope.WithHintf("The public key registered for issuer \"%s\" and subject \"%s\" is not allowed to request scope \"%s\".", claims.Issuer, claims.Subject, scope))
		}
	}

	if claims.ID != "" {
		if err := c.Storage.MarkJWTUsedForTime(ctx, claims.ID, claims.Expiry.Time()); err != nil {
			return errorsx.WithStack(fosite.ErrServerError.WithWrap(err).WithDebug(err.Error()))
		}
	}

	for _, scope := range request.GetRequestedScopes() {
		request.GrantScope(scope)
	}

	for _, audience := range claims.Audience {
		request.GrantAudience(audience)
	}

	session, err := c.getSessionFromRequest(request)
	if err != nil {
		return err
	}

	atLifespan := fosite.GetEffectiveLifespan(request.GetClient(), fosite.GrantTypeJwtBearer, fosite.AccessToken, c.HandleHelper.AccessTokenLifespan)
	session.SetExpiresAt(fosite.AccessToken, time.Now().UTC().Add(atLifespan).Round(time.Second))
	session.SetSubject(claims.Subject)

	return nil
}

func (c *Handler) PopulateTokenEndpointResponse(ctx context.Context, request fosite.AccessRequester, response fosite.AccessResponder) error {
	if err := c.CheckRequest(request); err != nil {
		return err
	}

	atLifespan := fosite.GetEffectiveLifespan(request.GetClient(), fosite.GrantTypeJwtBearer, fosite.AccessToken, c.HandleHelper.AccessTokenLifespan)
	return c.IssueAccessToken(ctx, atLifespan, request, response)
}

func (c *Handler) CanSkipClientAuth(requester fosite.AccessRequester) bool {
	return c.SkipClientAuth
}

func (c *Handler) CanHandleTokenEndpointRequest(requester fosite.AccessRequester) bool {
	// grant_type REQUIRED.
	// Value MUST be set to "authorization_code"
	return requester.GetGrantTypes().ExactOne(grantTypeJWTBearer)
}

func (c *Handler) CheckRequest(request fosite.AccessRequester) error {
	if !c.CanHandleTokenEndpointRequest(request) {
		return errorsx.WithStack(fosite.ErrUnknownRequest)
	}

	// Client Authentication is optional:
	//
	// Authentication of the client is optional, as described in
	//   Section 3.2.1 of OAuth 2.0 [RFC6749] and consequently, the
	//   "client_id" is only needed when a form of client authentication that
	//   relies on the parameter is used.

	// if client is authenticated, check grant types
	if !c.CanSkipClientAuth(request) && !request.GetClient().GetGrantTypes().Has(grantTypeJWTBearer) {
		return errorsx.WithStack(fosite.ErrUnauthorizedClient.WithHintf("The OAuth 2.0 Client is not allowed to use authorization grant \"%s\".", grantTypeJWTBearer))
	}

	return nil
}

func (c *Handler) validateTokenPreRequisites(token *jwt.JSONWebToken) error {
	unverifiedClaims := jwt.Claims{}
	if err := token.UnsafeClaimsWithoutVerification(&unverifiedClaims); err != nil {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("Looks like there are no claims in JWT in \"assertion\" request parameter.").
			WithWrap(err).WithDebug(err.Error()),
		)
	}
	if unverifiedClaims.Issuer == "" {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("The JWT in \"assertion\" request parameter MUST contain an \"iss\" (issuer) claim."),
		)
	}
	if unverifiedClaims.Subject == "" {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("The JWT in \"assertion\" request parameter MUST contain a \"sub\" (subject) claim."),
		)
	}

	return nil
}

func (c *Handler) findPublicKeyForToken(ctx context.Context, token *jwt.JSONWebToken) (*jose.JSONWebKey, error) {
	unverifiedClaims := jwt.Claims{}
	if err := token.UnsafeClaimsWithoutVerification(&unverifiedClaims); err != nil {
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithWrap(err).WithDebug(err.Error()))
	}

	var keyID string
	for _, header := range token.Headers {
		if header.KeyID != "" {
			keyID = header.KeyID
			break
		}
	}

	keyNotFoundErr := fosite.ErrInvalidGrant.WithHintf(
		"No public JWK was registered for issuer \"%s\" and subject \"%s\", and public key is required to check signature of JWT in \"assertion\" request parameter.",
		unverifiedClaims.Issuer,
		unverifiedClaims.Subject,
	)
	if keyID != "" {
		key, err := c.Storage.GetPublicKey(ctx, unverifiedClaims.Issuer, unverifiedClaims.Subject, keyID)
		if err != nil {
			return nil, errorsx.WithStack(keyNotFoundErr.WithWrap(err).WithDebug(err.Error()))
		}
		return key, nil
	}

	keys, err := c.Storage.GetPublicKeys(ctx, unverifiedClaims.Issuer, unverifiedClaims.Subject)
	if err != nil {
		return nil, errorsx.WithStack(keyNotFoundErr.WithWrap(err).WithDebug(err.Error()))
	}

	claims := jwt.Claims{}
	for _, key := range keys.Keys {
		err := token.Claims(key, &claims)
		if err == nil {
			return &key, nil
		}
	}

	return nil, errorsx.WithStack(keyNotFoundErr)
}

func (c *Handler) validateTokenClaims(ctx context.Context, claims jwt.Claims, key *jose.JSONWebKey) error {
	if len(claims.Audience) == 0 {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("The JWT in \"assertion\" request parameter MUST contain an \"aud\" (audience) claim."),
		)
	}

	if !claims.Audience.Contains(c.TokenURL) {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHintf(
				"The JWT in \"assertion\" request parameter MUST contain an \"aud\" (audience) claim containing a value \"%s\" that identifies the authorization server as an intended audience.",
				c.TokenURL,
			),
		)
	}

	if claims.Expiry == nil {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("The JWT in \"assertion\" request parameter MUST contain an \"exp\" (expiration time) claim."),
		)
	}

	if claims.Expiry.Time().Before(time.Now()) {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("The JWT in \"assertion\" request parameter expired."),
		)
	}

	if claims.NotBefore != nil && !claims.NotBefore.Time().Before(time.Now()) {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHintf(
				"The JWT in \"assertion\" request parameter contains an \"nbf\" (not before) claim, that identifies the time '%s' before which the token MUST NOT be accepted.",
				claims.NotBefore.Time().Format(time.RFC3339),
			),
		)
	}

	if !c.JWTIssuedDateOptional && claims.IssuedAt == nil {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("The JWT in \"assertion\" request parameter MUST contain an \"iat\" (issued at) claim."),
		)
	}

	var issuedDate time.Time
	if claims.IssuedAt != nil {
		issuedDate = claims.IssuedAt.Time()
	} else {
		issuedDate = time.Now()
	}
	if claims.Expiry.Time().Sub(issuedDate) > c.JWTMaxDuration {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHintf(
				"The JWT in \"assertion\" request parameter contains an \"exp\" (expiration time) claim with value \"%s\" that is unreasonably far in the future, considering token issued at \"%s\".",
				claims.Expiry.Time().Format(time.RFC3339),
				issuedDate.Format(time.RFC3339),
			),
		)
	}

	if !c.JWTIDOptional && claims.ID == "" {
		return errorsx.WithStack(fosite.ErrInvalidGrant.
			WithHint("The JWT in \"assertion\" request parameter MUST contain an \"jti\" (JWT ID) claim."),
		)
	}

	if claims.ID != "" {
		used, err := c.Storage.IsJWTUsed(ctx, claims.ID)
		if err != nil {
			return errorsx.WithStack(fosite.ErrServerError.WithWrap(err).WithDebug(err.Error()))
		}
		if used {
			return errorsx.WithStack(fosite.ErrJTIKnown)
		}
	}

	return nil
}

type extendedSession interface {
	Session
	fosite.Session
}

func (c *Handler) getSessionFromRequest(requester fosite.AccessRequester) (extendedSession, error) {
	session := requester.GetSession()
	if jwtSession, ok := session.(extendedSession); !ok {
		return nil, errorsx.WithStack(
			fosite.ErrServerError.WithHintf("Session must be of type *rfc7523.Session but got type: %T", session),
		)
	} else {
		return jwtSession, nil
	}
}
