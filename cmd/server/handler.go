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
 */

package server

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/context"
	"github.com/julienschmidt/httprouter"
	"github.com/meatballhat/negroni-logrus"
	"github.com/ory/go-convenience/corsx"
	"github.com/ory/graceful"
	"github.com/ory/herodot"
	"github.com/ory/hydra/client"
	"github.com/ory/hydra/config"
	"github.com/ory/hydra/consent"
	"github.com/ory/hydra/health"
	"github.com/ory/hydra/jwk"
	"github.com/ory/hydra/oauth2"
	"github.com/ory/hydra/pkg"
	"github.com/ory/metrics-middleware"
	"github.com/pkg/errors"
	"github.com/rs/cors"
	"github.com/spf13/cobra"
	"github.com/urfave/negroni"
)

var _ = &consent.Handler{}

func RunHost(c *config.Config) func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		fmt.Println(banner)

		router := httprouter.New()
		logger := c.GetLogger()
		w := herodot.NewJSONWriter(logger)
		w.ErrorEnhancer = nil

		serverHandler := &Handler{
			Config: c,
			H:      w,
		}
		serverHandler.registerRoutes(router)
		c.ForceHTTP, _ = cmd.Flags().GetBool("dangerous-force-http")

		if !c.ForceHTTP {
			if c.Issuer == "" {
				logger.Fatalln("IssuerURL must be explicitly specified unless --dangerous-force-http is passed. To find out more, use `hydra help host`.")
			}
			issuer, err := url.Parse(c.Issuer)
			pkg.Must(err, "Could not parse issuer URL: %s", err)
			if issuer.Scheme != "https" {
				logger.Fatalln("IssuerURL must use HTTPS unless --dangerous-force-http is passed. To find out more, use `hydra help host`.")
			}
		}

		n := negroni.New()
		n.Use(negronilogrus.NewMiddlewareFromLogger(logger, c.Issuer))
		n.Use(c.GetPrometheusMetrics())

		if ok, _ := cmd.Flags().GetBool("disable-telemetry"); !ok {
			c.GetLogger().Println("Transmission of telemetry data is enabled, to learn more go to: https://www.ory.sh/docs/guides/latest/telemetry/")

			enable := !(c.DatabaseURL == "" || c.DatabaseURL == "memory" || c.Issuer == "" || strings.Contains(c.Issuer, "localhost"))
			m := metrics.NewMetricsManager(
				metrics.Hash(c.Issuer+"|"+c.DatabaseURL),
				enable,
				"h8dRH3kVCWKkIFWydBmWsyYHR4M0u0vr",
				[]string{
					client.ClientsHandlerPath,
					jwk.KeyHandlerPath,
					jwk.WellKnownKeysPath,
					oauth2.DefaultConsentPath,
					oauth2.TokenPath,
					oauth2.AuthPath,
					oauth2.UserinfoPath,
					oauth2.WellKnownPath,
					oauth2.IntrospectPath,
					oauth2.RevocationPath,
					consent.ConsentPath,
					consent.LoginPath,
					health.AliveCheckPath,
					health.ReadyCheckPath,
					health.VersionPath,
					health.MetricsPrometheusPath,
					"/health/status",
					"/",
				},
				logger,
				"ory-hydra",
			)
			go m.RegisterSegment(c.BuildVersion, c.BuildHash, c.BuildTime)
			go m.CommitMemoryStatistics()
			n.Use(m)
		}

		n.UseFunc(serverHandler.rejectInsecureRequests)
		n.UseHandler(router)
		corsHandler := cors.New(corsx.ParseOptions()).Handler(n)

		var srv = graceful.WithDefaults(&http.Server{
			Addr:    c.GetAddress(),
			Handler: context.ClearHandler(corsHandler),
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{getOrCreateTLSCertificate(cmd, c)},
			},
		})

		err := graceful.Graceful(func() error {
			var err error
			logger.Infof("Setting up http server on %s", c.GetAddress())
			if c.ForceHTTP {
				logger.Warnln("HTTPS disabled. Never do this in production.")
				err = srv.ListenAndServe()
			} else if c.AllowTLSTermination != "" {
				logger.Infoln("TLS termination enabled, disabling https.")
				err = srv.ListenAndServe()
			} else {
				err = srv.ListenAndServeTLS("", "")
			}

			return err
		}, srv.Shutdown)
		logger.WithError(err).Fatal("Could not gracefully run server")
	}
}

type Handler struct {
	Clients *client.Handler
	Keys    *jwk.Handler
	OAuth2  *oauth2.Handler
	Consent *consent.Handler
	Config  *config.Config
	H       herodot.Writer
}

func (h *Handler) registerRoutes(router *httprouter.Router) {
	c := h.Config
	ctx := c.Context()

	// Set up dependencies
	injectJWKManager(c)
	clientsManager := newClientManager(c)
	injectConsentManager(c, clientsManager)

	injectFositeStore(c, clientsManager)
	oauth2Provider := newOAuth2Provider(c)

	// Set up handlers
	h.Clients = newClientHandler(c, router, clientsManager)
	h.Keys = newJWKHandler(c, router)
	h.Consent = newConsentHandler(c, router)
	h.OAuth2 = newOAuth2Handler(c, router, ctx.ConsentManager, oauth2Provider)
	_ = newHealthHandler(c, router)
}

func (h *Handler) rejectInsecureRequests(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if r.TLS != nil || h.Config.ForceHTTP {
		next.ServeHTTP(rw, r)
		return
	}

	if err := h.Config.DoesRequestSatisfyTermination(r); err == nil {
		next.ServeHTTP(rw, r)
		return
	} else {
		h.Config.GetLogger().WithError(err).Warnln("Could not serve http connection")
	}

	h.H.WriteErrorCode(rw, r, http.StatusBadGateway, errors.New("Can not serve request over insecure http"))
}
