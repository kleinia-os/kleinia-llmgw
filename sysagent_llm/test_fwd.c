// SPDX-License-Identifier: Apache-2.0
/*
 * test_fwd — offline unit tests for the fwd HTTP layer and the sys.llm
 * skill handlers, plus a --probe mode used by test_e2e.sh against a live
 * llmgwd.
 *
 *   ./test_fwd                              offline suite (stub server)
 *   ./test_fwd --probe HOST:PORT TOKEN [MODEL]   e2e probe vs llmgwd
 *
 * sysagent_llm.c is #included with SYSAGENT_LLM_NO_MAIN so the skill
 * handlers (gw_call, shape_reply, token loading) are tested directly,
 * without any ACB kernel / libagent dependency.  The offline stub is a
 * forked child of this binary serving canned responses on a port in the
 * 18480-18499 range.
 */
#define SYSAGENT_LLM_NO_MAIN
#include "sysagent_llm.c"

#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static int failures, checks;
static pid_t stub_pid;
static volatile int *stub_count;

static void check(int cond, const char *fmt, ...)
	__attribute__((format(printf, 2, 3)));
static void check(int cond, const char *fmt, ...)
{
	va_list ap;

	checks++;
	printf("%s - ", cond ? "ok" : "FAIL");
	va_start(ap, fmt);
	vprintf(fmt, ap);
	va_end(ap);
	printf("\n");
	if (!cond)
		failures++;
}

static int has(const char *hay, const char *needle)
{
	return strstr(hay, needle) != NULL;
}

/* ------------------------------------------------------------------ */
/* stub server (child process)                                         */

static int stub_write_all(int fd, const void *buf, size_t len)
{
	const char *p = buf;

	while (len) {
		ssize_t n = write(fd, p, len);

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

static void respond_hdr(int fd, int status, const char *reason,
			const char *ct, const char *extra_name,
			const char *extra_val, const char *body)
{
	char head[512];

	if (extra_name && extra_val)
		snprintf(head, sizeof(head),
			 "HTTP/1.1 %d %s\r\nContent-Type: %s\r\n"
			 "%s: %s\r\nContent-Length: %zu\r\n"
			 "Connection: close\r\n\r\n",
			 status, reason, ct, extra_name, extra_val,
			 strlen(body));
	else
		snprintf(head, sizeof(head),
			 "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %zu\r\n"
			 "Connection: close\r\n\r\n",
			 status, reason, ct, strlen(body));
	stub_write_all(fd, head, strlen(head));
	stub_write_all(fd, body, strlen(body));
}

static void respond(int fd, int status, const char *reason, const char *ct,
		    const char *body)
{
	respond_hdr(fd, status, reason, ct, NULL, NULL, body);
}

/* the id llmgw stamps on metered chat replies (req_ + 16 hex) */
#define STUB_REQ_ID "req_deadbeef1234abcd"

/* case-insensitive header lookup inside [buf, buf+hdr_end) */
static int stub_header(const char *buf, size_t hdr_end, const char *name,
		       char *out, size_t outsz)
{
	size_t nlen = strlen(name);
	size_t i = 0;

	while (i < hdr_end) {
		size_t lend = i;
		size_t colon;

		while (lend < hdr_end && buf[lend] != '\n')
			lend++;
		colon = i;
		while (colon < lend && buf[colon] != ':')
			colon++;
		if (colon < lend && colon - i == nlen &&
		    !strncasecmp(buf + i, name, nlen)) {
			size_t s = colon + 1, e = lend;

			while (s < e && (buf[s] == ' ' || buf[s] == '\t'))
				s++;
			if (e > s && buf[e - 1] == '\r')
				e--;
			if (e - s >= outsz)
				e = s + outsz - 1;
			memcpy(out, buf + s, e - s);
			out[e - s] = '\0';
			return 1;
		}
		i = lend + 1;
	}
	return 0;
}

static void stub_serve_conn(int fd)
{
	char req[8192], method[16], path[128];
	char v[256], v2[256], body[4096];
	size_t len = 0, hdr_end = 0;
	struct timeval tv = { .tv_sec = 5, .tv_usec = 0 };
	int got_head = 0, i;

	setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
	while (len < sizeof(req) - 1) {
		ssize_t n;
		size_t cl, need;

		if (got_head) {
			if (stub_header(req, hdr_end, "Content-Length", v,
					sizeof(v))) {
				cl = (size_t)strtoul(v, NULL, 10);
				need = hdr_end + 4 + cl;
				if (len >= need)
					break;
			} else {
				break;		/* no body expected */
			}
		}
		n = read(fd, req + len, sizeof(req) - 1 - len);
		if (n <= 0)
			break;
		len += (size_t)n;
		req[len] = '\0';
		for (i = 0; !got_head && i + 3 < (int)len; i++) {
			if (req[i] == '\r' && req[i + 1] == '\n' &&
			    req[i + 2] == '\r' && req[i + 3] == '\n') {
				hdr_end = (size_t)i;
				got_head = 1;
				break;
			}
		}
	}
	if (!got_head)
		return;
	(*stub_count)++;
	if (sscanf(req, "%15s %127s", method, path) != 2)
		return;
	{
		size_t body_start = hdr_end + 4;
		size_t blen = len > body_start ? len - body_start : 0;

		/* cap the echoed body well below out[] so the snprintfs
		 * below can never overflow */
		if (blen > 1024)
			blen = 1024;
		memcpy(body, req + body_start, blen);
		body[blen] = '\0';

		if (!strcmp(path, "/ok")) {
			respond(fd, 200, "OK", "application/json", "{\"ok\":true}");
		} else if (!strcmp(path, "/plain")) {
			stub_write_all(fd,
				"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n"
				"Connection: close\r\n\r\nplain-til-eof",
				sizeof("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n"
				       "Connection: close\r\n\r\nplain-til-eof") - 1);
		} else if (!strcmp(path, "/chunked")) {
			const char chunked[] =
				"HTTP/1.1 200 OK\r\n"
				"Transfer-Encoding: chunked\r\n"
				"Connection: close\r\n\r\n"
				"5\r\nhello\r\n"
				"6\r\n world\r\n"
				"8\r\n! chunks\r\n"
				"0\r\n\r\n";

			stub_write_all(fd, chunked, sizeof(chunked) - 1);
		} else if (!strcmp(path, "/echo")) {
			char out[4096];
			int n = 0;

			n += snprintf(out + n, sizeof(out) - (size_t)n,
				      "internal=%s\n",
				      stub_header(req, hdr_end, "x-kln-internal",
						  v, sizeof(v)) ? v : "MISSING");
			n += snprintf(out + n, sizeof(out) - (size_t)n,
				      "aid=%s\n",
				      stub_header(req, hdr_end, "x-kln-aid",
						  v, sizeof(v)) ? v : "MISSING");
			n += snprintf(out + n, sizeof(out) - (size_t)n,
				      "agent=%s\n",
				      stub_header(req, hdr_end, "x-kln-agent",
						  v, sizeof(v)) ? v : "MISSING");
			n += snprintf(out + n, sizeof(out) - (size_t)n,
				      "ctype=%s\n",
				      stub_header(req, hdr_end, "Content-Type",
						  v, sizeof(v)) ? v : "MISSING");
			n += snprintf(out + n, sizeof(out) - (size_t)n,
				      "clen=%s\n",
				      stub_header(req, hdr_end, "Content-Length",
						  v, sizeof(v)) ? v : "MISSING");
			snprintf(out + n, sizeof(out) - (size_t)n, "body=%s",
				 body);
			respond(fd, 200, "OK", "text/plain", out);
		} else if (!strcmp(path, "/status404")) {
			respond(fd, 404, "Not Found", "application/json",
				"{\"error\":{\"message\":\"not found\"}}");
		} else if (!strcmp(path, "/status500")) {
			respond(fd, 500, "Internal Server Error", "text/plain",
				"upstream exploded");
		} else if (!strcmp(path, "/slow")) {
			sleep(2);	/* client must hit its recv timeout */
			respond(fd, 200, "OK", "text/plain", "finally");
		} else if (!strcmp(path, "/count")) {
			char num[32];

			snprintf(num, sizeof(num), "%d", *stub_count);
			respond(fd, 200, "OK", "text/plain", num);
		} else if (!strcmp(path, "/healthz")) {
			respond(fd, 200, "OK", "application/json", "{\"ok\":true}");
		} else if (!strcmp(path, "/v1/models")) {
			respond(fd, 200, "OK", "application/json",
				"{\"object\":\"list\",\"data\":[]}");
		} else if (!strcmp(path, "/internal/usage")) {
			if (stub_header(req, hdr_end, "x-kln-internal", v,
					sizeof(v)) && v[0])
				respond(fd, 200, "OK", "application/json",
					"{\"since\":\"2026\",\"by_key\":{}}");
			else
				respond(fd, 401, "Unauthorized",
					"application/json",
					"{\"error\":\"internal token required\"}");
		} else if (!strcmp(path, "/reqid")) {
			respond_hdr(fd, 200, "OK", "application/json",
				    "x-kln-req-id", STUB_REQ_ID,
				    "{\"ok\":true}");
		} else if (!strcmp(path, "/reqid-junk")) {
			/* hostile header value: capture must sanitize to JSON-safe */
			respond_hdr(fd, 200, "OK", "application/json",
				    "x-kln-req-id", "req_abc\"; drop table--x",
				    "{\"ok\":true}");
		} else if (!strcmp(path, "/v1/chat/completions")) {
			if (!stub_header(req, hdr_end, "x-kln-internal", v,
					 sizeof(v)) || !v[0]) {
				respond(fd, 401, "Unauthorized",
					"application/json",
					"{\"error\":{\"message\":\"no internal token\"}}");
			} else if (has(body, "failme")) {
				respond_hdr(fd, 429, "Too Many Requests",
					"application/json",
					"x-kln-req-id", STUB_REQ_ID,
					"{\"error\":{\"type\":\"rate_limited\","
					"\"message\":\"slow down\"}}");
			} else {
				respond_hdr(fd, 200, "OK", "application/json",
					"x-kln-req-id", STUB_REQ_ID,
					"{\"choices\":[{\"message\":"
					"\"role\":\"assistant\","
					"\"content\":\"pong\"}}]}");
			}
		} else {
			respond(fd, 404, "Not Found", "text/plain", "no route");
		}
	}
}

static int stub_run(int portpipe_w)
{
	struct sockaddr_in sa;
	int lfd = -1, port = 0, p, one = 1;

	signal(SIGPIPE, SIG_IGN);
	signal(SIGCHLD, SIG_IGN);
	setsid();			/* own group: kill(-pid) reaps workers */
	for (p = 18481; p <= 18490; p++) {
		lfd = socket(AF_INET, SOCK_STREAM, 0);
		if (lfd < 0)
			continue;
		setsockopt(lfd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
		memset(&sa, 0, sizeof(sa));
		sa.sin_family = AF_INET;
		sa.sin_port = htons((unsigned short)p);
		inet_pton(AF_INET, "127.0.0.1", &sa.sin_addr);
		if (bind(lfd, (struct sockaddr *)&sa, sizeof(sa)) == 0) {
			port = p;
			break;
		}
		close(lfd);
	}
	(void)write(portpipe_w, &port, sizeof(port));
	if (!port)
		_exit(1);
	if (listen(lfd, 16) < 0)
		_exit(1);
	for (;;) {
		int cfd = accept(lfd, NULL, NULL);

		if (cfd < 0) {
			if (errno == EINTR)
				continue;
			_exit(1);
		}
		if (fork() == 0) {
			close(lfd);
			stub_serve_conn(cfd);
			close(cfd);
			_exit(0);
		}
		close(cfd);
	}
}

static int stub_start(void)
{
	int pipefd[2], port = 0;

	if (pipe(pipefd) < 0) {
		perror("pipe");
		return -1;
	}
	stub_count = mmap(NULL, sizeof(int), PROT_READ | PROT_WRITE,
			  MAP_SHARED | MAP_ANONYMOUS, -1, 0);
	if (stub_count == MAP_FAILED) {
		perror("mmap");
		return -1;
	}
	*stub_count = 0;
	stub_pid = fork();
	if (stub_pid < 0) {
		perror("fork");
		return -1;
	}
	if (stub_pid == 0) {
		close(pipefd[0]);
		stub_run(pipefd[1]);
		_exit(1);
	}
	close(pipefd[1]);
	if (read(pipefd[0], &port, sizeof(port)) != (ssize_t)sizeof(port) ||
	    !port) {
		fprintf(stderr, "stub failed to bind 18481-18490\n");
		return -1;
	}
	close(pipefd[0]);
	return port;
}

static void stub_stop(void)
{
	if (stub_pid > 0)
		kill(-stub_pid, SIGKILL);	/* group: workers included */
	if (stub_pid > 0)
		waitpid(stub_pid, NULL, 0);
}

/* ------------------------------------------------------------------ */
/* test helpers                                                        */

static int stub_port;

/* point gw at the stub with a fixed token (no file loading) */
static void gw_to_stub(int use_token)
{
	memset(&gw, 0, sizeof(gw));
	gw.port = stub_port;
	if (use_token)
		strcpy(gw.token, "unit-test-token");
	gw.token_loaded = 1;	/* never try to load a token file here */
}

static int fwd_get(const char *path, int tmo, struct fwd_result *r)
{
	return fwd_request("GET", "127.0.0.1", stub_port, path, NULL, 0,
			   NULL, NULL, 0, tmo, r);
}

/* ------------------------------------------------------------------ */
/* 1. fwd layer against the stub                                       */

static void suite_fwd(void)
{
	struct fwd_result r;

	printf("--- fwd: basics\n");
	check(fwd_get("/ok", 5000, &r) == 0 && r.status == 200 &&
	      r.body_len == strlen("{\"ok\":true}") &&
	      !strcmp(r.body, "{\"ok\":true}") && !r.error[0],
	      "GET /ok: status 200, exact body, Content-Length framing");
	fwd_result_free(&r);

	check(fwd_get("/plain", 5000, &r) == 0 && r.status == 200 &&
	      !strcmp(r.body, "plain-til-eof"),
	      "GET /plain: body read to EOF (no Content-Length)");
	fwd_result_free(&r);

	check(fwd_get("/chunked", 5000, &r) == 0 && r.status == 200 &&
	      !strcmp(r.body, "hello world! chunks"),
	      "GET /chunked: chunked body reassembled");
	fwd_result_free(&r);

	check(fwd_get("/status404", 5000, &r) == 0 && r.status == 404 &&
	      has(r.body, "not found"),
	      "GET /status404: error status + body surface");
	fwd_result_free(&r);

	printf("--- fwd: POST + identity headers\n");
	{
		const char *hn[] = { "x-kln-internal", "x-kln-aid",
				     "x-kln-agent", "Content-Type" };
		const char *hv[] = { "testtoken-abc123", "4242",
				     "sys.llm-caller", "application/json" };
		const char *payload =
			"{\"model\":\"m1\",\"messages\":[]}";
		char clen[32], btag[128];
		int ok, rc;

		snprintf(clen, sizeof(clen), "clen=%zu", strlen(payload));
		snprintf(btag, sizeof(btag), "body=%s", payload);
		rc = fwd_request("POST", "127.0.0.1", stub_port, "/echo",
				 payload, strlen(payload), hn, hv, 4,
				 5000, &r);
		ok = rc == 0 && r.status == 200 &&
		     has(r.body, "internal=testtoken-abc123") &&
		     has(r.body, "aid=4242") &&
		     has(r.body, "agent=sys.llm-caller") &&
		     has(r.body, "ctype=application/json") &&
		     has(r.body, clen) && has(r.body, btag);
		check(ok, "POST /echo: headers + byte-identical body");
		fwd_result_free(&r);
	}

	printf("--- fwd: x-kln-req-id capture\n");
	check(fwd_get("/reqid", 5000, &r) == 0 && r.status == 200 &&
	      !strcmp(r.req_id, STUB_REQ_ID),
	      "GET /reqid: x-kln-req-id captured verbatim");
	fwd_result_free(&r);

	check(fwd_get("/ok", 5000, &r) == 0 && r.status == 200 &&
	      !r.req_id[0],
	      "GET /ok: no header -> empty req_id (nothing correlates)");
	fwd_result_free(&r);

	check(fwd_get("/reqid-junk", 5000, &r) == 0 && r.status == 200 &&
	      !strcmp(r.req_id, "req_abcdroptable--x"),
	      "hostile header value sanitized to [A-Za-z0-9_.-]");
	fwd_result_free(&r);

	printf("--- fwd: timeout\n");
	{
		time_t t0 = time(NULL);

		check(fwd_get("/slow", 300, &r) < 0 && r.status == 0 &&
		      has(r.error, "timeout"),
		      "GET /slow with 300ms timeout: recv timeout");
		fwd_result_free(&r);
		check(time(NULL) - t0 < 2, "timeout fired quickly");
	}

	printf("--- fwd: header injection + validation\n");
	{
		const char *hn[] = { "x-kln-internal", "Host",
				     "Good-Header", "Bad Name" };
		const char *hv[] = { "abc\r\nx-evil: 1", "evil.example",
				     "fine", "x" };
		int c1 = -1, c2 = -1, rc = 0;
		struct fwd_result e;

		/* baseline request count (assign first: check() args may be
		 * evaluated before the condition) */
		if (fwd_get("/count", 5000, &e) == 0)
			sscanf(e.body, "%d", &c1);
		fwd_result_free(&e);
		check(c1 >= 0, "baseline /count = %d", c1);

		rc |= fwd_request("GET", "127.0.0.1", stub_port, "/ok",
				  NULL, 0, hn, hv, 1, 5000, &e) == 0;
		fwd_result_free(&e);		/* value with CR/LF */
		rc |= fwd_request("GET", "127.0.0.1", stub_port, "/ok\r\nX: 1",
				  NULL, 0, NULL, NULL, 0, 5000, &e) == 0;
		fwd_result_free(&e);		/* CRLF in path */
		rc |= fwd_request("GET /x", "127.0.0.1", stub_port, "/ok",
				  NULL, 0, NULL, NULL, 0, 5000, &e) == 0;
		fwd_result_free(&e);		/* space in method */
		rc |= fwd_request("GET", "127.0.0.1", stub_port, "/ok",
				  NULL, 0, &hn[1], &hv[1], 1, 5000, &e) == 0;
		fwd_result_free(&e);		/* Host override */
		rc |= fwd_request("GET", "127.0.0.1", stub_port, "/ok",
				  NULL, 0, &hn[3], &hv[3], 1, 5000, &e) == 0;
		fwd_result_free(&e);		/* space in header name */
		check(rc == 0, "all malformed requests rejected client-side");

		if (fwd_get("/count", 5000, &e) == 0)
			sscanf(e.body, "%d", &c2);
		fwd_result_free(&e);
		check(c2 == c1 + 1,
		      "no rejected request reached the stub (%d -> %d)",
		      c1, c2);
	}

	printf("--- fwd: loopback-only + refused\n");
	{
		struct fwd_result e;

		check(fwd_request("GET", "localhost", stub_port, "/ok", NULL,
				  0, NULL, NULL, 0, 5000, &e) < 0 &&
		      has(e.error, "loopback"),
		      "host 'localhost' refused (only 127.0.0.1)");
		fwd_result_free(&e);
		check(fwd_request("GET", "10.0.0.1", 80, "/ok", NULL, 0,
				  NULL, NULL, 0, 5000, &e) < 0 &&
		      has(e.error, "loopback"),
		      "host '10.0.0.1' refused");
		fwd_result_free(&e);
	}
	{
		struct sockaddr_in sa;
		socklen_t sl = sizeof(sa);
		int fd = socket(AF_INET, SOCK_STREAM, 0), p;
		struct fwd_result e;

		memset(&sa, 0, sizeof(sa));
		sa.sin_family = AF_INET;
		sa.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
		sa.sin_port = 0;
		if (bind(fd, (struct sockaddr *)&sa, sizeof(sa)) == 0 &&
		    getsockname(fd, (struct sockaddr *)&sa, &sl) == 0) {
			p = ntohs(sa.sin_port);
			close(fd);
			check(fwd_request("GET", "127.0.0.1", p, "/ok", NULL,
					  0, NULL, NULL, 0, 2000, &e) < 0 &&
			      e.status == 0 && e.error[0] &&
			      has(e.error, "connect"),
			      "connection refused -> transport error");
			fwd_result_free(&e);
		} else {
			close(fd);
			check(0, "could not allocate a probe port");
		}
	}
}

/* ------------------------------------------------------------------ */
/* 2. reply shaping + token loading (pure logic)                       */

static void suite_shape(void)
{
	char out[ACB_REQ_MAX];
	struct fwd_result r;

	printf("--- shape_reply\n");
	memset(&r, 0, sizeof(r));
	strcpy(r.error, "connect 127.0.0.1:1: Connection refused");
	shape_reply(&r, out, sizeof(out));
	check(has(out, "gateway_unreachable") &&
	      has(out, "\"gateway_status\":0"),
	      "transport error envelope: %s", out);

	r.status = 401;
	r.body = strdup("{\"error\":\"internal token required\"}");
	r.body_len = strlen(r.body);
	shape_reply(&r, out, sizeof(out));
	check(!strcmp(out,
		      "{\"error\":\"internal token required\",\"gateway_status\":401}"),
	      "non-2xx JSON object spliced with gateway_status");
	free(r.body);

	r.status = 500;
	r.body = strdup("upstream exploded");
	r.body_len = strlen(r.body);
	shape_reply(&r, out, sizeof(out));
	check(has(out, "\"gateway_status\":500") &&
	      has(out, "upstream exploded"),
	      "non-2xx non-JSON body escaped into error envelope");
	free(r.body);

	r.status = 503;
	r.body = strdup("{}");
	r.body_len = 2;
	shape_reply(&r, out, sizeof(out));
	check(!strcmp(out, "{\"gateway_status\":503}"), "empty JSON object");
	free(r.body);

	r.status = 502;
	r.body = strdup("{\"error\":{\"x\":1}}  \n");
	r.body_len = strlen(r.body);
	shape_reply(&r, out, sizeof(out));
	check(!strcmp(out, "{\"error\":{\"x\":1},\"gateway_status\":502}"),
	      "trailing whitespace trimmed before splicing");
	free(r.body);

	r.status = 200;
	r.body = malloc(5000);
	memset(r.body, 'a', 5000);
	r.body_len = 5000;
	shape_reply(&r, out, sizeof(out));
	check(has(out, "agentfs_limit") &&
	      has(out, "\"gateway_status\":200") && strlen(out) < 4000,
	      "oversized 2xx -> agentfs_limit envelope (no truncation)");
	free(r.body);

	r.status = 200;
	r.body = strdup("plain-ok");
	r.body_len = strlen(r.body);
	shape_reply(&r, out, sizeof(out));
	check(!strcmp(out, "plain-ok"), "2xx small body verbatim");
	free(r.body);
	memset(&r, 0, sizeof(r));

	printf("--- shape_reply: gateway_req_id\n");
	strcpy(r.req_id, STUB_REQ_ID);

	r.status = 200;
	r.body = strdup("{\"choices\":[{}]}");
	r.body_len = strlen(r.body);
	shape_reply(&r, out, sizeof(out));
	check(!strcmp(out, "{\"choices\":[{}],\"gateway_req_id\":\"" STUB_REQ_ID "\"}"),
	      "2xx JSON object body spliced with gateway_req_id");
	free(r.body);

	r.body = strdup("plain-ok");	/* non-object: nothing to splice into */
	r.body_len = strlen(r.body);
	shape_reply(&r, out, sizeof(out));
	check(!strcmp(out, "plain-ok"),
	      "2xx non-object body stays verbatim despite id");
	free(r.body);

	/* exact fit: inner + rid suffix + '}' lands on cap-1 */
	r.body = malloc(4055);
	memset(r.body, 'a', 4055);
	r.body[0] = '{';
	r.body[4054] = '}';
	r.body_len = 4055;
	shape_reply(&r, out, sizeof(out));
	check(out[0] == '{' && has(out, STUB_REQ_ID) && strlen(out) == 4095,
	      "2xx object splice boundary: id fits exactly at the cap");
	free(r.body);

	/* one byte more and the splice cannot fit: id wins, content degrades */
	r.body = malloc(4095);
	memset(r.body, 'a', 4095);
	r.body[0] = '{';
	r.body[4094] = '}';
	r.body_len = 4095;
	shape_reply(&r, out, sizeof(out));
	check(has(out, "agentfs_limit") && has(out, STUB_REQ_ID),
	      "2xx object with no splice room: agentfs_limit envelope carries the id");
	free(r.body);

	r.status = 429;
	r.body = strdup("{\"error\":{\"type\":\"rate_limited\"}}");
	r.body_len = strlen(r.body);
	shape_reply(&r, out, sizeof(out));
	check(!strcmp(out, "{\"error\":{\"type\":\"rate_limited\"},"
			   "\"gateway_status\":429,"
			   "\"gateway_req_id\":\"" STUB_REQ_ID "\"}"),
	      "non-2xx object: gateway_status and gateway_req_id together");
	free(r.body);

	r.status = 503;
	r.body = strdup("{}");
	r.body_len = 2;
	shape_reply(&r, out, sizeof(out));
	check(!strcmp(out, "{\"gateway_status\":503,"
			   "\"gateway_req_id\":\"" STUB_REQ_ID "\"}"),
	      "empty object error: id present");
	free(r.body);
	memset(&r, 0, sizeof(r));

	printf("--- json_escape\n");
	{
		char eb[64];
		const char *in = "a\"b\\c\nd\x01z";

		json_escape(eb, sizeof(eb), in, strlen(in));
		check(!strcmp(eb, "a\\\"b\\\\c\\nd\\u0001z"),
		      "quotes/backslash/newline/control escaped: %s", eb);
	}

	printf("--- token file loading\n");
	{
		char path[] = "/tmp/sysagent_llm_test_tokenXXXXXX";
		int fd = mkstemp(path);
		FILE *tf;
		int saved;
		char logbuf[4096];

		check(fd >= 0, "mkstemp token file");
		if (fd < 0)
			return;
		write(fd, "tok123\n", 7);	/* trailing newline must be trimmed */
		fchmod(fd, 0600);
		close(fd);

		memset(&gw, 0, sizeof(gw));
		strcpy(gw.token_file, path);
		token_ensure();
		check(gw.token_loaded == 1 && !strcmp(gw.token, "tok123"),
		      "0600 token file loaded, trailing newline trimmed");

		/* missing file: warn once, stay unloaded, retry later */
		tf = tmpfile();
		saved = dup(2);
		fflush(stderr);
		dup2(fileno(tf), 2);
		memset(&gw, 0, sizeof(gw));
		strcpy(gw.token_file, "/nonexistent/token");
		token_ensure();
		fflush(stderr);
		dup2(saved, 2);
		close(saved);
		rewind(tf);
		logbuf[read(fileno(tf), logbuf, sizeof(logbuf) - 1)] = '\0';
		check(gw.token_loaded == 0 && has(logbuf, "retrying"),
		      "missing token file: warning + retry-later semantics");

		/* wrong mode: warn but still load */
		chmod(path, 0644);
		tf = tmpfile();
		saved = dup(2);
		fflush(stderr);
		dup2(fileno(tf), 2);
		memset(&gw, 0, sizeof(gw));
		strcpy(gw.token_file, path);
		token_ensure();
		fflush(stderr);
		dup2(saved, 2);
		close(saved);
		rewind(tf);
		logbuf[read(fileno(tf), logbuf, sizeof(logbuf) - 1)] = '\0';
		check(gw.token_loaded == 1 && !strcmp(gw.token, "tok123") &&
		      has(logbuf, "not 0600"),
		      "0644 token file: warned, still loaded");

		/* control characters: never usable as a header value */
		fd = open(path, O_WRONLY | O_TRUNC);
		write(fd, "bad\x01token\n", 9);
		close(fd);
		chmod(path, 0600);
		tf = tmpfile();
		saved = dup(2);
		fflush(stderr);
		dup2(fileno(tf), 2);
		memset(&gw, 0, sizeof(gw));
		strcpy(gw.token_file, path);
		token_ensure();
		fflush(stderr);
		dup2(saved, 2);
		close(saved);
		rewind(tf);
		logbuf[read(fileno(tf), logbuf, sizeof(logbuf) - 1)] = '\0';
		check(gw.token_loaded == 0 && has(logbuf, "control"),
		      "control chars in token file -> ignored");
		unlink(path);
	}
}

/* ------------------------------------------------------------------ */
/* 3. skill handlers end-to-end vs the stub                            */

static void suite_skills(void)
{
	char out[ACB_REQ_MAX];
	const char *req = "{\"model\":\"m1\",\"messages\":"
			  "[{\"role\":\"user\",\"content\":\"hi\"}]}";
	int n;

	printf("--- skill handlers vs stub\n");
	gw_to_stub(1);

	n = skill_health(out, sizeof(out), NULL, 0, 0);
	check(n == (int)strlen("{\"ok\":true}") &&
	      !strcmp(out, "{\"ok\":true}"), "health: verbatim body");

	n = skill_models(out, sizeof(out), NULL, 0, 0);
	check(has(out, "\"object\":\"list\"") && !has(out, "gateway_req_id"),
	      "models: verbatim body, no id (no usage row correlates)");

	n = skill_usage(out, sizeof(out), NULL, 0, 4242);
	check(has(out, "since") && !has(out, "gateway_status") &&
	      !has(out, "gateway_req_id"),
	      "usage with token: 200 body verbatim");

	n = skill_chat(out, sizeof(out), req, strlen(req), 4242);
	check(has(out, "pong") && !has(out, "gateway_status") &&
	      has(out, "\"gateway_req_id\":\"" STUB_REQ_ID "\""),
	      "chat 2xx: body + gateway_req_id spliced in");

	n = skill_chat(out, sizeof(out), "{\"model\":\"failme\"}",
		      strlen("{\"model\":\"failme\"}"), 7);
	check(!strcmp(out,
		      "{\"error\":{\"type\":\"rate_limited\",\"message\":"
		      "\"slow down\"},\"gateway_status\":429,"
		      "\"gateway_req_id\":\"" STUB_REQ_ID "\"}"),
	      "chat non-2xx JSON: {\"error\":...,\"gateway_status\","
	      "\"gateway_req_id\"}");

	gw_to_stub(0);			/* no token -> no internal header */
	n = skill_usage(out, sizeof(out), NULL, 0, 0);
	check(!strcmp(out,
		      "{\"error\":\"internal token required\","
		      "\"gateway_status\":401}"),
	      "usage without token: wrapped 401");

	n = skill_chat(out, sizeof(out), req, strlen(req), 0);
	check(has(out, "\"gateway_status\":401"),
	      "chat without token: wrapped 401");

	n = skill_chat(out, sizeof(out), "not json", 8, 0);
	check(has(out, "invalid_request") && has(out, "\"gateway_status\":0"),
	      "chat with non-JSON payload: local invalid_request");
}

/* ------------------------------------------------------------------ */
/* probe mode: vs a live llmgwd (test_e2e.sh)                          */

static int probe_mode(const char *hostport, const char *token,
		      const char *model)
{
	char url[128], out[ACB_REQ_MAX];
	char saved[256];
	int i, n;

	snprintf(url, sizeof(url), "http://%s", hostport);
	if (gw_parse_base(url) < 0)
		return 2;

	/* inject the token directly instead of a file */
	snprintf(gw.token, sizeof(gw.token), "%s", token);
	gw.token_loaded = 1;
	strcpy(saved, gw.token);

	/* wait for llmgwd to come up (up to ~10s) */
	for (i = 0; i < 100; i++) {
		n = skill_health(out, sizeof(out), NULL, 0, 0);
		if (n > 0 && has(out, "\"ok\""))
			break;
		usleep(100000);
	}
	check(i < 100, "healthz: 200 {\"ok\":true} (after %d retries)", i);

	n = skill_models(out, sizeof(out), NULL, 0, 0);
	check(n > 0 && out[0] == '{' && has(out, "\"object\":\"list\""),
	      "models: 200 JSON list body");

	n = skill_usage(out, sizeof(out), NULL, 0, 0);
	check(n > 0 && out[0] == '{' && has(out, "\"since\""),
	      "usage with internal token: 200 JSON");

	/* negative control: stripping the token must yield a wrapped 401 */
	gw.token[0] = '\0';
	n = skill_usage(out, sizeof(out), NULL, 0, 0);
	check(has(out, "\"gateway_status\":401"),
	      "usage without token: wrapped 401 (x-kln-internal is real)");
	strcpy(gw.token, saved);

	if (model) {
		char req[512];

		snprintf(req, sizeof(req),
			 "{\"model\":\"%s\",\"messages\":[{\"role\":\"user\","
			 "\"content\":\"ping\"}],\"max_tokens\":8}", model);
		n = skill_chat(out, sizeof(out), req, strlen(req), 12345);
		if (has(out, "\"choices\"")) {
			const char *rid = strstr(out, "\"gateway_req_id\":\"");

			check(rid && !strncmp(rid + 18, "req_", 4),
			      "chat via internal auth: live completion + "
			      "gateway_req_id correlates (%s)",
			      rid ? rid + 18 : "missing");
		} else {
			const char *gs = strstr(out, "\"gateway_status\":");
			int code = gs ? atoi(gs + 17) : 0;

			check(gs && code >= 400 && out[0] == '{',
			      "chat via internal auth: upstream failure "
			      "wrapped as {error,gateway_status:%d}", code);
		}
	}

	printf("probe: %d checks, %d failures\n", checks, failures);
	return failures ? 1 : 0;
}

int main(int argc, char **argv)
{
	if (argc >= 4 && !strcmp(argv[1], "--probe"))
		return probe_mode(argv[2], argv[3], argc > 4 ? argv[4] : NULL);
	if (argc != 1) {
		fprintf(stderr,
			"usage: test_fwd [--probe HOST:PORT TOKEN [MODEL]]\n");
		return 2;
	}

	stub_port = stub_start();
	if (stub_port < 0)
		return 1;
	printf("stub listening on 127.0.0.1:%d\n", stub_port);

	suite_fwd();
	suite_shape();
	suite_skills();

	stub_stop();
	printf("%d checks, %d failures\n", checks, failures);
	return failures ? 1 : 0;
}
