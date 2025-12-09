package utils

import "os"

var (
	Env                    = os.Getenv("ENV")
	Env_TracingServiceName = os.Getenv("TRACING_SERVICE_NAME")
	Env_OLTPEndpoint       = os.Getenv("OLTP_ENDPOINT")

	PG_DSN = os.Getenv("PG_DSN")

	TLSKey  = GetEnvOrDefault("TLS_KEY", "key.pem")
	TLSCert = GetEnvOrDefault("TLS_CERT", "cert.pem")
)
