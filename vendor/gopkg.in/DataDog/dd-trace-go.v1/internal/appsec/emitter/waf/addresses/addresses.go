// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package addresses

const (
	ServerRequestMethodAddr            = "server.request.method"
	ServerRequestRawURIAddr            = "server.request.uri.raw"
	ServerRequestHeadersNoCookiesAddr  = "server.request.headers.no_cookies"
	ServerRequestCookiesAddr           = "server.request.cookies"
	ServerRequestQueryAddr             = "server.request.query"
	ServerRequestPathParamsAddr        = "server.request.path_params"
	ServerRequestBodyAddr              = "server.request.body"
	ServerResponseStatusAddr           = "server.response.status"
	ServerResponseHeadersNoCookiesAddr = "server.response.headers.no_cookies"

	ClientIPAddr = "http.client_ip"

	UserIDAddr           = "usr.id"
	UserSessionIDAddr    = "usr.session_id"
	UserLoginSuccessAddr = "server.business_logic.users.login.success"
	UserLoginFailureAddr = "server.business_logic.users.login.failure"

	ServerIoNetURLAddr    = "server.io.net.url"
	ServerIOFSFileAddr    = "server.io.fs.file"
	ServerDBStatementAddr = "server.db.statement"
	ServerDBTypeAddr      = "server.db.system"

	GRPCServerMethodAddr                   = "grpc.server.method"
	GRPCServerRequestMetadataAddr          = "grpc.server.request.metadata"
	GRPCServerRequestMessageAddr           = "grpc.server.request.message"
	GRPCServerResponseMessageAddr          = "grpc.server.response.message"
	GRPCServerResponseMetadataHeadersAddr  = "grpc.server.response.metadata.headers"
	GRPCServerResponseMetadataTrailersAddr = "grpc.server.response.metadata.trailers"
	GRPCServerResponseStatusCodeAddr       = "grpc.server.response.status"

	GraphQLServerResolverAddr = "graphql.server.resolver"
)
