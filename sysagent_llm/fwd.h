/* SPDX-License-Identifier: Apache-2.0 */
/*
 * fwd — minimal hand-rolled HTTP/1.1 client for the sys.llm skill carrier.
 *
 * Deliberately tiny (no libcurl): single-shot GET/POST over a fresh TCP
 * connection, loopback targets only, Content-Length / chunked /
 * read-to-EOF body framing, SO_SNDTIMEO/SO_RCVTIMEO timeouts, and a
 * header-injection guard that validates every header value before the
 * socket is even created.  No TLS, no keep-alive, no redirects, no SSE.
 */
#ifndef FWD_H
#define FWD_H

#include <stddef.h>

/* response body hard cap, bytes */
#define FWD_MAX_BODY (256 * 1024)

/* Result of one HTTP exchange.  status==0 means the exchange never
 * completed at the HTTP level (connect/timeout/parse failure) and
 * error[] says why.  body is heap-allocated and NUL-terminated; free it
 * with fwd_result_free(). */
struct fwd_result {
	int status;		/* HTTP status code, 0 on transport error */
	char *body;		/* malloc'd response body, may be NULL */
	size_t body_len;
	char error[192];	/* transport error text, "" when ok */
	char req_id[48];	/* x-kln-req-id response header, "" when the
				 * gateway did not stamp one (no usage row
				 * correlates with this exchange). Kept to
				 * [A-Za-z0-9_.-] so it is always JSON-safe
				 * and header-injection-proof. */
};

/* Perform one HTTP/1.1 request to host:port.
 *
 *   method:  "GET" or "POST" (validated)
 *   host:    MUST be the literal "127.0.0.1" — the carrier only ever
 *            talks to the local llmgwd; any other host is refused
 *   port:    1..65535
 *   path:    must start with '/', no control chars/spaces
 *   body:    request payload (may be NULL when body_len==0); sent with
 *            Content-Length exactly as given, byte-identical
 *   hdr_names/hdr_values: n_headers extra header pairs (may be NULL
 *            when n_headers==0).  Values are validated: any control
 *            character makes the call fail BEFORE any socket is opened
 *            (CR/LF header-injection guard).  Overriding Host,
 *            Content-Length or Connection is refused.
 *   timeout_ms: applied to send() and recv() via SO_SNDTIMEO/SO_RCVTIMEO
 *
 * Returns 0 when an HTTP response was parsed (r->status set, r->body
 * owned by the caller, r->req_id from the x-kln-req-id response header
 * when present); -1 on transport/validation error (r->status==0,
 * r->error set).  On return, release r->body with fwd_result_free().
 */
int fwd_request(const char *method, const char *host, int port,
		const char *path, const void *body, size_t body_len,
		const char *const *hdr_names, const char *const *hdr_values,
		int n_headers, int timeout_ms, struct fwd_result *r);

/* Free r->body and zero the struct. */
void fwd_result_free(struct fwd_result *r);

#endif /* FWD_H */
