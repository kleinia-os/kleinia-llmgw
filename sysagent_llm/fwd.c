// SPDX-License-Identifier: Apache-2.0
/*
 * fwd — minimal HTTP/1.1 client (see fwd.h).  One connection per request
 * (Connection: close), so body framing can be resolved after reading the
 * response to EOF: Content-Length wins, chunked is decoded, anything
 * else is "body is whatever came after the header block".
 */
#define _GNU_SOURCE
#include "fwd.h"

#include <arpa/inet.h>
#include <errno.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

#define FWD_LOCALHOST	"127.0.0.1"		/* only trusted target */
#define FWD_UA		"sysagent_llm/1.0"
#define FWD_HDR_MAX	(16 * 1024)		/* response header block cap */
#define FWD_REQ_MAX	(16 * 1024)		/* request head cap */

static void set_err(struct fwd_result *r, const char *fmt, ...)
	__attribute__((format(printf, 2, 3)));
static void set_err(struct fwd_result *r, const char *fmt, ...)
{
	va_list ap;

	va_start(ap, fmt);
	vsnprintf(r->error, sizeof(r->error), fmt, ap);
	va_end(ap);
}

/* token for method/path/header-name: nonempty, printable, no spaces */
static int token_ok(const char *s)
{
	if (!s || !*s)
		return 0;
	for (; *s; s++) {
		unsigned char c = (unsigned char)*s;

		if (c <= 0x20 || c == 0x7f)
			return 0;
	}
	return 1;
}

/* header value: printable only — rejecting all control chars is the
 * header-injection guard (CR/LF can never smuggle in an extra header) */
static int header_value_ok(const char *v)
{
	if (!v)
		return 0;
	for (; *v; v++) {
		unsigned char c = (unsigned char)*v;

		if (c < 0x20 || c == 0x7f)
			return 0;
	}
	return 1;
}

static int reserved_header(const char *name)
{
	return !strcasecmp(name, "Host") ||
	       !strcasecmp(name, "Content-Length") ||
	       !strcasecmp(name, "Connection") ||
	       !strcasecmp(name, "Transfer-Encoding");
}

static int write_all(int fd, const void *buf, size_t len)
{
	const char *p = buf;

	while (len) {
		ssize_t n = send(fd, p, len, MSG_NOSIGNAL);

		if (n < 0) {
			if (errno == EINTR)
				continue;
			return -1;
		}
		p += n;
		len -= (size_t)n;
	}
	return 0;
}

/* Read the whole response to EOF into a NUL-terminated heap buffer.
 * The request always carries Connection: close, so EOF delimits it. */
static char *read_all(int fd, size_t *out_len, char *err, size_t errsz)
{
	size_t cap = 8192, len = 0, hard = FWD_HDR_MAX + FWD_MAX_BODY + 4096;
	char *buf = malloc(cap);

	while (buf) {
		ssize_t n;

		if (len + 1 >= cap) {
			char *nb;

			if (cap >= hard) {
				snprintf(err, errsz, "response too large (cap %zu)", hard);
				free(buf);
				return NULL;
			}
			cap *= 2;
			if (cap > hard)
				cap = hard;
			nb = realloc(buf, cap);
			if (!nb) {
				free(buf);
				return NULL;
			}
			buf = nb;
		}
		n = recv(fd, buf + len, cap - len - 1, 0);
		if (n == 0)
			break;			/* clean EOF */
		if (n < 0) {
			if (errno == EINTR)
				continue;
			if (errno == EAGAIN || errno == EWOULDBLOCK)
				snprintf(err, errsz, "recv timeout");
			else
				snprintf(err, errsz, "recv: %s", strerror(errno));
			free(buf);
			return NULL;
		}
		len += (size_t)n;
	}
	if (!buf) {
		snprintf(err, errsz, "out of memory");
		return NULL;
	}
	buf[len] = '\0';
	*out_len = len;
	return buf;
}

/* find "\r\n\r\n"; *hdr_end = offset of the leading '\r' */
static int find_header_end(const char *buf, size_t len, size_t *hdr_end)
{
	size_t i;

	if (len < 4)
		return -1;
	for (i = 0; i + 3 < len; i++) {
		if (buf[i] == '\r' && buf[i + 1] == '\n' &&
		    buf[i + 2] == '\r' && buf[i + 3] == '\n') {
			*hdr_end = i;
			return 0;
		}
	}
	return -1;
}

static int parse_status(const char *buf, int *status)
{
	int maj, min, code, used = -1;

	if (sscanf(buf, "HTTP/%d.%d %d%n", &maj, &min, &code, &used) != 3 ||
	    used <= 0 || code < 100 || code > 599)
		return -1;
	*status = code;
	return 0;
}

/* case-insensitive single-header lookup within [hdrs, hdrs+hdr_len);
 * trimmed value copied into out (NUL-terminated).  1 = found. */
static int header_get(const char *hdrs, size_t hdr_len, const char *name,
		      char *out, size_t outsz)
{
	size_t nlen = strlen(name);
	const char *end = hdrs + hdr_len;
	const char *line = hdrs;

	while (line < end) {
		const char *eol = memchr(line, '\n', (size_t)(end - line));
		const char *lend = eol ? eol : end;
		const char *colon = memchr(line, ':', (size_t)(lend - line));

		if (colon && (size_t)(colon - line) == nlen &&
		    !strncasecmp(line, name, nlen)) {
			const char *v = colon + 1;
			size_t vlen;

			while (v < lend && (*v == ' ' || *v == '\t'))
				v++;
			vlen = (size_t)(lend - v);
			while (vlen && (v[vlen - 1] == '\r' || v[vlen - 1] == ' ' ||
					v[vlen - 1] == '\t'))
				vlen--;
			if (vlen >= outsz)
				vlen = outsz - 1;
			memcpy(out, v, vlen);
			out[vlen] = '\0';
			return 1;
		}
		line = eol ? eol + 1 : end;
	}
	return 0;
}

/* Copy the x-kln-req-id header value into r->req_id, keeping only
 * [A-Za-z0-9_.-]: the value is spliced into JSON replies verbatim, so
 * it must never carry quotes, backslashes or control characters, and a
 * hostile/misbehaving loopback server must not smuggle anything past it. */
static void capture_req_id(const char *hdrs, size_t hdr_len,
			   struct fwd_result *r)
{
	char v[sizeof(r->req_id)];
	size_t i, n = 0;

	if (!header_get(hdrs, hdr_len, "x-kln-req-id", v, sizeof(v)))
		return;
	for (i = 0; v[i] && n + 1 < sizeof(r->req_id); i++) {
		unsigned char c = (unsigned char)v[i];

		if ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		    (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-')
			r->req_id[n++] = (char)c;
	}
	r->req_id[n] = '\0';
}

/* minimal chunked decoder over the already fully-buffered body */
static int dechunk(const char *src, size_t len, char **out, size_t *out_len,
		   char *err, size_t errsz)
{
	size_t cap = 4096, olen = 0, pos = 0;
	char *buf = malloc(cap);

	while (buf) {
		const char *eol;
		char tmp[32], *endp;
		size_t slen, csize;

		eol = memchr(src + pos, '\n', len - pos);
		if (!eol)
			goto bad;
		slen = (size_t)(eol - (src + pos));
		if (slen && src[pos + slen - 1] == '\r')
			slen--;
		if (slen == 0 || slen >= sizeof(tmp))
			goto bad;
		memcpy(tmp, src + pos, slen);
		tmp[slen] = '\0';
		csize = strtoul(tmp, &endp, 16);
		if (endp == tmp || (*endp != '\0' && *endp != ';'))
			goto bad;
		pos = (size_t)(eol - src) + 1;
		if (csize == 0)
			break;			/* last chunk (trailers ignored) */
		if (pos + csize + 2 > len)
			goto bad;
		if (olen + csize + 1 > cap) {
			char *nb;

			while (olen + csize + 1 > cap)
				cap *= 2;
			nb = realloc(buf, cap);
			if (!nb) {
				free(buf);
				snprintf(err, errsz, "out of memory");
				return -1;
			}
			buf = nb;
		}
		memcpy(buf + olen, src + pos, csize);
		olen += csize;
		pos += csize;
		if (src[pos] == '\r')
			pos++;
		if (src[pos] == '\n')
			pos++;
	}
	if (!buf) {
		snprintf(err, errsz, "out of memory");
		return -1;
	}
	buf[olen] = '\0';
	*out = buf;
	*out_len = olen;
	return 0;
bad:
	free(buf);
	snprintf(err, errsz, "malformed chunked body");
	return -1;
}

void fwd_result_free(struct fwd_result *r)
{
	if (!r)
		return;
	free(r->body);
	r->body = NULL;
	r->body_len = 0;
	r->status = 0;
	r->error[0] = '\0';
	r->req_id[0] = '\0';
}

int fwd_request(const char *method, const char *host, int port,
		const char *path, const void *body, size_t body_len,
		const char *const *hdr_names, const char *const *hdr_values,
		int n_headers, int timeout_ms, struct fwd_result *r)
{
	char req[FWD_REQ_MAX];
	struct sockaddr_in sa;
	struct timeval tv;
	char *raw = NULL;
	size_t raw_len = 0, hdr_end, body_start, blen = 0;
	int fd = -1, i, n, status = 0;
	char *body_out = NULL;
	int ret = -1;

	memset(r, 0, sizeof(*r));

	/* ---- validate everything BEFORE any socket is touched ---- */
	if (!token_ok(method)) {
		set_err(r, "bad method");
		return -1;
	}
	if (!path || path[0] != '/' || !token_ok(path)) {
		set_err(r, "bad path (must start with '/', no spaces/controls)");
		return -1;
	}
	if (!host || strcmp(host, FWD_LOCALHOST)) {
		set_err(r, "refusing non-loopback host '%s' "
			   "(the carrier may only talk to the local llmgwd)",
			host ? host : "(null)");
		return -1;
	}
	if (port < 1 || port > 65535) {
		set_err(r, "bad port %d", port);
		return -1;
	}
	if (body_len && !body) {
		set_err(r, "body NULL with nonzero length");
		return -1;
	}
	if (body_len > FWD_MAX_BODY) {
		set_err(r, "request body %zu exceeds cap %d", body_len, FWD_MAX_BODY);
		return -1;
	}
	for (i = 0; i < n_headers; i++) {
		if (!token_ok(hdr_names[i])) {
			set_err(r, "bad header name #%d", i);
			return -1;
		}
		if (!header_value_ok(hdr_values[i])) {
			set_err(r, "header value for '%s' rejected "
				   "(control chars / header injection)",
				hdr_names[i]);
			return -1;
		}
		if (reserved_header(hdr_names[i])) {
			set_err(r, "refusing to override reserved header '%s'",
				hdr_names[i]);
			return -1;
		}
	}
	if (timeout_ms <= 0) {
		set_err(r, "bad timeout %d ms", timeout_ms);
		return -1;
	}

	/* ---- build the request head ---- */
	n = snprintf(req, sizeof(req),
		     "%s %s HTTP/1.1\r\n"
		     "Host: %s:%d\r\n"
		     "Connection: close\r\n"
		     "User-Agent: %s\r\n",
		     method, path, host, port, FWD_UA);
	if (n < 0 || n >= (int)sizeof(req)) {
		set_err(r, "request head too large");
		return -1;
	}
	if (body_len) {
		n += snprintf(req + n, sizeof(req) - (size_t)n,
			      "Content-Length: %zu\r\n", body_len);
		if (n < 0 || n >= (int)sizeof(req)) {
			set_err(r, "request head too large");
			return -1;
		}
	}
	for (i = 0; i < n_headers; i++) {
		n += snprintf(req + n, sizeof(req) - (size_t)n, "%s: %s\r\n",
			      hdr_names[i], hdr_values[i]);
		if (n < 0 || n >= (int)sizeof(req)) {
			set_err(r, "request head too large");
			return -1;
		}
	}
	n += snprintf(req + n, sizeof(req) - (size_t)n, "\r\n");
	if (n < 0 || n >= (int)sizeof(req)) {
		set_err(r, "request head too large");
		return -1;
	}

	/* ---- connect (loopback only) with send/recv timeouts ---- */
	memset(&sa, 0, sizeof(sa));
	sa.sin_family = AF_INET;
	sa.sin_port = htons((unsigned short)port);
	if (inet_pton(AF_INET, host, &sa.sin_addr) != 1) {
		set_err(r, "bad host address");
		return -1;
	}
	fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) {
		set_err(r, "socket: %s", strerror(errno));
		return -1;
	}
	tv.tv_sec = timeout_ms / 1000;
	tv.tv_usec = (timeout_ms % 1000) * 1000;
	setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
	setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
	if (connect(fd, (struct sockaddr *)&sa, sizeof(sa)) < 0) {
		set_err(r, "connect %s:%d: %s", host, port, strerror(errno));
		close(fd);
		return -1;
	}

	/* ---- send head + body, read the full response ---- */
	if (write_all(fd, req, (size_t)n) < 0) {
		set_err(r, "send: %s", strerror(errno));
		goto out;
	}
	if (body_len && write_all(fd, body, body_len) < 0) {
		set_err(r, "send body: %s", strerror(errno));
		goto out;
	}
	raw = read_all(fd, &raw_len, r->error, sizeof(r->error));
	if (!raw)
		goto out;			/* r->error already set */

	if (find_header_end(raw, raw_len, &hdr_end) < 0) {
		set_err(r, "malformed response: no header terminator");
		goto out;
	}
	if (parse_status(raw, &status) < 0) {
		set_err(r, "malformed status line");
		goto out;
	}
	capture_req_id(raw, hdr_end, r);
	body_start = hdr_end + 4;

	{
		char v[64];
		size_t rest = raw_len - body_start;

		if (header_get(raw, hdr_end, "Content-Length", v, sizeof(v))) {
			char *endp;
			unsigned long cl = strtoul(v, &endp, 10);

			if (endp == v || *endp) {
				set_err(r, "bad Content-Length");
				goto out;
			}
			if (cl > rest) {
				set_err(r, "truncated body (Content-Length %lu, got %zu)",
					cl, rest);
				goto out;
			}
			blen = (size_t)cl;
			body_out = malloc(blen + 1);
			if (!body_out) {
				set_err(r, "out of memory");
				goto out;
			}
			memcpy(body_out, raw + body_start, blen);
			body_out[blen] = '\0';
		} else if (header_get(raw, hdr_end, "Transfer-Encoding", v,
				      sizeof(v)) && strcasestr(v, "chunked")) {
			if (dechunk(raw + body_start, rest, &body_out, &blen,
				    r->error, sizeof(r->error)) < 0)
				goto out;
		} else {
			/* neither: body runs to EOF */
			blen = rest;
			body_out = malloc(blen + 1);
			if (!body_out) {
				set_err(r, "out of memory");
				goto out;
			}
			memcpy(body_out, raw + body_start, blen);
			body_out[blen] = '\0';
		}
		if (blen > FWD_MAX_BODY) {
			free(body_out);
			body_out = NULL;
			set_err(r, "response body %zu exceeds cap %d",
				blen, FWD_MAX_BODY);
			goto out;
		}
	}

	r->status = status;
	r->body = body_out;
	r->body_len = blen;
	body_out = NULL;
	ret = 0;
out:
	free(body_out);
	free(raw);
	close(fd);
	return ret;
}
