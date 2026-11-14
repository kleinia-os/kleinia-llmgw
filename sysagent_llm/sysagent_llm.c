// SPDX-License-Identifier: Apache-2.0
/*
 * sysagent_llm — the sys.llm skill carrier (Kleinia ACB).
 *
 * Registers the ACB agent "sys.llm" (class SYS) with four skills and
 * forwards each agentfs invoke to the local llmgwd daemon over plain
 * HTTP on 127.0.0.1, attaching the kernel-attested caller identity:
 *
 *   models  GET  {llmgw}/v1/models             reply = body verbatim
 *   chat    POST {llmgw}/v1/chat/completions   payload = OpenAI chat
 *          request JSON (model uses a gateway alias); 2xx JSON object
 *          replies carry the gateway's request id spliced in as
 *          "gateway_req_id" (the x-kln-req-id response header — agentfs
 *          replies have no headers), anything else comes back as
 *          {"error": {...}, "gateway_status": <code>[, "gateway_req_id"]},
 *          both correlating 1:1 with the gateway usage row
 *   usage   GET  {llmgw}/internal/usage        internal token required
 *   health  GET  {llmgw}/healthz               reply = body verbatim
 *
 * Every forwarded request carries:
 *   x-kln-internal: <token>  shared secret, read once from --token-file
 *   x-kln-aid: <caller_aid>  from struct acb_recv (0 = unattached
 *                            caller, header omitted)
 *   x-kln-agent: sys.llm-caller
 *                            static label: the agentfs UAPI exposes the
 *                            caller AID but not the caller NAME
 *
 * agentfs replies are capped at ACB_REQ_MAX (4096) by the UAPI, so a
 * larger gateway response is returned as a JSON error envelope
 * ("agentfs_limit") instead of a silently truncated body.
 *
 * Usage:
 *   sysagent_llm [--llmgw http://127.0.0.1:8484]
 *                [--token-file /run/kleinia/llmgw-internal.token]
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#include "libagent.h"
#include "fwd.h"

#define AGENT_NAME		"sys.llm"
#define GW_HOST			"127.0.0.1"	/* enforced, loopback only */
#define DEFAULT_LLMGW		"http://127.0.0.1:8484"
#define DEFAULT_TOKEN_FILE	"/run/kleinia/llmgw-internal.token"
#define GW_CALLER_LABEL		"sys.llm-caller"
#define GW_TIMEOUT_MS		(30 * 1000)	/* per request, SO_RCVTIMEO */
#define CHAT_CTYPE		"application/json"

struct gw_cfg {
	int port;
	char token_file[380];
	char token[256];	/* trimmed secret; "" until loaded */
	int token_loaded;
};

static struct gw_cfg gw;
static int token_mode_warned, token_load_warned;

struct skill_def {
	const char *name;
	const char *schema;
	int (*fn)(char *out, size_t cap, const char *payload, size_t plen,
		  unsigned long long caller_aid);
};

/* ------------------------------------------------------------------ */
/* small helpers                                                       */

/* snprintf that returns the CLAMPED length (never past cap-1) */
static int snprintfc(char *out, size_t cap, const char *fmt, ...)
	__attribute__((format(printf, 3, 4)));
static int snprintfc(char *out, size_t cap, const char *fmt, ...)
{
	va_list ap;
	int n;

	va_start(ap, fmt);
	n = vsnprintf(out, cap, fmt, ap);
	va_end(ap);
	if (n < 0)
		return 0;
	if ((size_t)n >= cap)
		n = (int)cap - 1;
	return n;
}

/* JSON-escape s[0..len) into out; returns escaped length */
static size_t json_escape(char *out, size_t cap, const char *s, size_t len)
{
	size_t i, n = 0;

	for (i = 0; i < len; i++) {
		unsigned char c = (unsigned char)s[i];
		const char *rep = NULL;
		char ubuf[8];

		switch (c) {
		case '"':  rep = "\\\""; break;
		case '\\': rep = "\\\\"; break;
		case '\n': rep = "\\n"; break;
		case '\r': rep = "\\r"; break;
		case '\t': rep = "\\t"; break;
		case '\b': rep = "\\b"; break;
		case '\f': rep = "\\f"; break;
		default:
			if (c < 0x20 || c == 0x7f) {
				snprintf(ubuf, sizeof(ubuf), "\\u%04x", c);
				rep = ubuf;
			}
		}
		if (rep) {
			size_t rl = strlen(rep);

			if (n + rl + 1 > cap)
				break;
			memcpy(out + n, rep, rl);
			n += rl;
		} else {
			if (n + 2 > cap)
				break;
			out[n++] = (char)c;
		}
	}
	out[n] = '\0';
	return n;
}

/* ------------------------------------------------------------------ */
/* configuration: --llmgw base URL, --token-file                        */

/* accept exactly: http://127.0.0.1[:port][/] */
static int gw_parse_base(const char *url)
{
	const char *p, *colon, *slash;
	char host[64];
	int port = 80;

	if (strncmp(url, "http://", 7)) {
		fprintf(stderr,
			"error: --llmgw must start with http:// (no TLS): %s\n",
			url);
		return -1;
	}
	p = url + 7;
	colon = strchr(p, ':');
	slash = strchr(p, '/');
	if (colon && (!slash || colon < slash)) {
		char *endp;
		long v;

		if ((size_t)(colon - p) >= sizeof(host)) {
			fprintf(stderr, "error: host too long: %s\n", url);
			return -1;
		}
		memcpy(host, p, (size_t)(colon - p));
		host[colon - p] = '\0';
		v = strtol(colon + 1, &endp, 10);
		if (endp == colon + 1 || (*endp != '\0' && *endp != '/') ||
		    v < 1 || v > 65535) {
			fprintf(stderr, "error: bad port in %s\n", url);
			return -1;
		}
		port = (int)v;
	} else {
		size_t hlen = slash ? (size_t)(slash - p) : strlen(p);

		if (hlen >= sizeof(host)) {
			fprintf(stderr, "error: host too long: %s\n", url);
			return -1;
		}
		memcpy(host, p, hlen);
		host[hlen] = '\0';
	}
	if (strcmp(host, GW_HOST)) {
		fprintf(stderr,
			"error: --llmgw host must be %s (the carrier refuses "
			"non-loopback targets): %s\n", GW_HOST, host);
		return -1;
	}
	slash = strchr(p, '/');
	if (slash && slash[1] != '\0') {
		fprintf(stderr, "error: --llmgw must not contain a path: %s\n",
			url);
		return -1;
	}
	gw.port = port;
	return 0;
}

/* Read the shared secret once and cache it.  A trailing newline is
 * trimmed; llmgwd-created token files have none, and a newline can
 * never be part of an HTTP header value.  Warn (but continue) when the
 * file is not 0600.  Retried on the next invoke until it succeeds, so
 * llmgwd may be started after the carrier. */
static void token_ensure(void)
{
	int fd;
	struct stat st;
	ssize_t n;
	size_t len;

	if (gw.token_loaded)
		return;
	fd = open(gw.token_file, O_RDONLY);
	if (fd < 0) {
		if (!token_load_warned) {
			fprintf(stderr,
				"[sysagent_llm] warning: token file %s: %s "
				"(chat/usage will be unauthorized; retrying "
				"on next invoke)\n",
				gw.token_file, strerror(errno));
			token_load_warned = 1;
		}
		return;
	}
	if (fstat(fd, &st) == 0 && (st.st_mode & 0777) != 0600 &&
	    !token_mode_warned) {
		fprintf(stderr,
			"[sysagent_llm] warning: %s mode is %04o, not 0600 — "
			"the llmgw shared secret should not be group/other "
			"readable\n",
			gw.token_file, (unsigned)(st.st_mode & 0777));
		token_mode_warned = 1;
	}
	n = read(fd, gw.token, sizeof(gw.token) - 1);
	close(fd);
	if (n <= 0) {
		if (!token_load_warned) {
			fprintf(stderr,
				"[sysagent_llm] warning: token file %s is empty\n",
				gw.token_file);
			token_load_warned = 1;
		}
		return;
	}
	gw.token[n] = '\0';
	len = (size_t)n;
	while (len && (gw.token[len - 1] == '\n' || gw.token[len - 1] == '\r' ||
		       gw.token[len - 1] == ' ' || gw.token[len - 1] == '\t'))
		len--;
	gw.token[len] = '\0';
	if (len == 0) {
		if (!token_load_warned) {
			fprintf(stderr,
				"[sysagent_llm] warning: token file %s has no token\n",
				gw.token_file);
			token_load_warned = 1;
		}
		return;
	}
	/* an HTTP header value must be printable: anything else would be
	 * either an injection attempt or a misconfigured file */
	for (n = 0; n < (ssize_t)len; n++) {
		unsigned char c = (unsigned char)gw.token[n];

		if (c < 0x20 || c == 0x7f) {
			fprintf(stderr,
				"[sysagent_llm] warning: %s contains control "
				"characters — ignoring token file\n",
				gw.token_file);
			gw.token[0] = '\0';
			return;
		}
	}
	gw.token_loaded = 1;
	token_load_warned = 0;
}

/* ------------------------------------------------------------------ */
/* gateway call                                                        */

static int gw_call(const char *method, const char *path, const char *ctype,
		   const void *body, size_t body_len,
		   unsigned long long caller_aid, struct fwd_result *res)
{
	const char *hn[5], *hv[5];
	char aidbuf[32];
	int n = 0;

	token_ensure();
	if (gw.token[0]) {
		hn[n] = "x-kln-internal";
		hv[n] = gw.token;
		n++;
	}
	if (caller_aid) {
		snprintf(aidbuf, sizeof(aidbuf), "%llu", caller_aid);
		hn[n] = "x-kln-aid";
		hv[n] = aidbuf;
		n++;
	}
	hn[n] = "x-kln-agent";
	hv[n] = GW_CALLER_LABEL;
	n++;
	if (ctype) {
		hn[n] = "Content-Type";
		hv[n] = ctype;
		n++;
	}
	return fwd_request(method, GW_HOST, gw.port, path, body, body_len,
			   hn, hv, n, GW_TIMEOUT_MS, res);
}

/* ------------------------------------------------------------------ */
/* reply shaping                                                       */

/* Build the ,"gateway_req_id":"req_..." fragment spliced into JSON reply
 * objects whenever the gateway stamped a request id on its response. The
 * value correlates the agentfs reply with the gateway usage row — audit
 * log, journald line and (G1) cnode llm_call segment all share it. Empty
 * when the gateway sent no id (transport failure, or an endpoint that
 * writes no usage row, e.g. models/usage/health). */
static void rid_suffix(const struct fwd_result *res, char *out, size_t cap)
{
	out[0] = '\0';
	if (res->req_id[0])
		snprintf(out, cap, ",\"gateway_req_id\":\"%s\"", res->req_id);
}

/* Shape one gateway exchange into an agentfs reply (cap = ACB_REQ_MAX):
 *   transport failure          {"error":{"type":"gateway_unreachable",...},
 *                               "gateway_status":0}
 *   2xx JSON object, fits      body + "gateway_req_id" spliced in when the
 *                               gateway stamped one (agentfs replies have
 *                               no headers, so the id rides as a field;
 *                               when even this small splice would not
 *                               fit, the id wins and the reply becomes
 *                               agentfs_limit — "object 2xx ⇒ correlatable
 *                               id" has no silent exceptions)
 *   2xx, anything else, fits   body verbatim (non-object bodies, e.g. an
 *                               SSE stream transcript, have nothing to
 *                               splice into)
 *   2xx, too big               {"error":{"type":"agentfs_limit",...},
 *                               "gateway_status":<code>[,gateway_req_id]}
 *   non-2xx JSON object body   body + "gateway_status"[,gateway_req_id]
 *   non-2xx anything else      {"error":{"message":...},
 *                               "gateway_status":N[,gateway_req_id]}
 */
static int shape_reply(const struct fwd_result *res, char *out, size_t cap)
{
	char rid[64];

	rid_suffix(res, rid, sizeof(rid));

	if (res->status == 0) {
		char ebuf[512];

		json_escape(ebuf, sizeof(ebuf), res->error, strlen(res->error));
		return snprintfc(out, cap,
				 "{\"error\":{\"type\":\"gateway_unreachable\","
				 "\"message\":\"%s\"},\"gateway_status\":0}",
				 ebuf);
	}

	if (res->status >= 200 && res->status < 300) {
		if (res->body_len < cap) {
			const char *b = res->body ? res->body : "";
			size_t s = 0, e = res->body_len;
			int is_obj;

			while (s < e && (b[s] == ' ' || b[s] == '\t' ||
					 b[s] == '\r' || b[s] == '\n'))
				s++;
			while (e > s && (b[e - 1] == ' ' || b[e - 1] == '\t' ||
					 b[e - 1] == '\r' || b[e - 1] == '\n'))
				e--;
			is_obj = (e > s + 2 && b[s] == '{' && b[e - 1] == '}');

			if (is_obj && rid[0]) {
				size_t inner = e - s - 1;	/* without '}' */

				if (inner + strlen(rid) + 1 <= cap - 1) {
					/* {"choices":...} ->
					 * {"choices":...,"gateway_req_id":...} */
					int n;

					memcpy(out, b + s, inner);
					n = (int)inner;
					n += snprintfc(out + n,
						       cap - (size_t)n,
						       "%s}", rid);
					return n;
				}
				/* splice would not fit: deterministic contract
				 * over marginal content */
				return snprintfc(out, cap,
						 "{\"error\":{\"type\":"
						 "\"agentfs_limit\","
						 "\"message\":\"gateway response of %zu bytes "
						 "exceeds the agentfs reply limit of %zu "
						 "bytes\"},\"gateway_status\":%d%s}",
						 res->body_len, cap - 1,
						 res->status, rid);
			}
			memcpy(out, res->body, res->body_len);
			out[res->body_len] = '\0';
			return (int)res->body_len;
		}
		return snprintfc(out, cap,
				 "{\"error\":{\"type\":\"agentfs_limit\","
				 "\"message\":\"gateway response of %zu bytes "
				 "exceeds the agentfs reply limit of %zu "
				 "bytes\"},\"gateway_status\":%d%s}",
				 res->body_len, cap - 1, res->status, rid);
	}

	{
		const char *b = res->body ? res->body : "";
		size_t blen = res->body ? res->body_len : 0;
		size_t s = 0, e = blen;
		int is_obj, empty_obj, fits;

		while (s < e && (b[s] == ' ' || b[s] == '\t' ||
				 b[s] == '\r' || b[s] == '\n'))
			s++;
		while (e > s && (b[e - 1] == ' ' || b[e - 1] == '\t' ||
				 b[e - 1] == '\r' || b[e - 1] == '\n'))
			e--;

		empty_obj = (e == s + 2 && b[s] == '{' && b[s + 1] == '}');
		is_obj = (e > s + 2 && b[s] == '{' && b[e - 1] == '}');
		fits = is_obj && (e - s) + 32 < cap;

		if (fits) {
			/* {"error": ...} -> {"error": ...,"gateway_status":N} */
			size_t inner = e - s - 1;	/* without closing '}' */
			int n;

			memcpy(out, b + s, inner);	/* '{' ... inner */
			n = (int)inner;
			n += snprintfc(out + n, cap - (size_t)n,
				       ",\"gateway_status\":%d%s}",
				       res->status, rid);
			return n;
		}
		if (empty_obj)
			/* "{}" has nothing to splice onto */
			return snprintfc(out, cap, "{\"gateway_status\":%d%s}",
					 res->status, rid);
		if (is_obj) {
			/* JSON object but too big to splice: keep it valid */
			return snprintfc(out, cap,
					 "{\"error\":{\"type\":"
					 "\"agentfs_limit\",\"message\":"
					 "\"gateway response of %zu bytes "
					 "exceeds the agentfs reply limit of "
					 "%zu bytes\"},\"gateway_status\":%d%s}",
					 res->body_len, cap - 1, res->status,
					 rid);
		}
		{
			char ebuf[512];

			json_escape(ebuf, sizeof(ebuf), b + s, e - s);
			return snprintfc(out, cap,
					 "{\"error\":{\"message\":\"%s\"},"
					 "\"gateway_status\":%d%s}",
					 ebuf, res->status, rid);
		}
	}
}

/* ------------------------------------------------------------------ */
/* skill handlers                                                      */

static int skill_models(char *out, size_t cap, const char *payload,
			size_t plen, unsigned long long aid)
{
	struct fwd_result res;
	int n;

	(void)payload;
	(void)plen;
	gw_call("GET", "/v1/models", NULL, NULL, 0, aid, &res);
	n = shape_reply(&res, out, cap);
	fwd_result_free(&res);
	return n;
}

static int skill_chat(char *out, size_t cap, const char *payload,
		      size_t plen, unsigned long long aid)
{
	struct fwd_result res;
	size_t s = 0, e = plen;
	int n;

	while (s < e && (payload[s] == ' ' || payload[s] == '\t' ||
			 payload[s] == '\r' || payload[s] == '\n'))
		s++;
	if (s == e || payload[s] != '{') {
		return snprintfc(out, cap,
				 "{\"error\":{\"type\":\"invalid_request\","
				 "\"message\":\"chat payload must be an OpenAI "
				 "chat/completions JSON request body\"},"
				 "\"gateway_status\":0}");
	}
	gw_call("POST", "/v1/chat/completions", CHAT_CTYPE,
		payload, plen, aid, &res);
	n = shape_reply(&res, out, cap);
	fwd_result_free(&res);
	return n;
}

static int skill_usage(char *out, size_t cap, const char *payload,
		       size_t plen, unsigned long long aid)
{
	struct fwd_result res;
	int n;

	(void)payload;
	(void)plen;
	gw_call("GET", "/internal/usage", NULL, NULL, 0, aid, &res);
	n = shape_reply(&res, out, cap);
	fwd_result_free(&res);
	return n;
}

static int skill_health(char *out, size_t cap, const char *payload,
			size_t plen, unsigned long long aid)
{
	struct fwd_result res;
	int n;

	(void)payload;
	(void)plen;
	gw_call("GET", "/healthz", NULL, NULL, 0, aid, &res);
	n = shape_reply(&res, out, cap);
	fwd_result_free(&res);
	return n;
}

static struct skill_def skills[] = {
	{ "models", "{}", skill_models },
	{ "chat",
	  "{\"model\":\"string (gateway alias)\","
	  "\"messages\":\"array\",\"stream\":\"bool?\"}",
	  skill_chat },
	{ "usage",  "{}", skill_usage },
	{ "health", "{}", skill_health },
	{ NULL, NULL, NULL },
};

/* ------------------------------------------------------------------ */

/* main() and usage() are compiled out when sysagent_llm.c is #included
 * by test_fwd.c (SYSAGENT_LLM_NO_MAIN) — the test reuses the handlers
 * above against stub/live gateways without any kernel dependency. */
#ifndef SYSAGENT_LLM_NO_MAIN
static void usage(FILE *f)
{
	fprintf(f,
		"usage: sysagent_llm [--llmgw http://127.0.0.1:8484] "
		"[--token-file %s]\n"
		"\n"
		"Registers the ACB agent \"%s\" (skills: models, chat, usage, "
		"health) and\n"
		"forwards each agentfs skill invoke to the local llmgw daemon "
		"over HTTP.\n"
		"The gateway base URL must be loopback; the caller AID from "
		"agentfs is\n"
		"forwarded as the x-kln-aid header.\n",
		DEFAULT_TOKEN_FILE, AGENT_NAME);
}

int main(int argc, char **argv)
{
	const char *base = DEFAULT_LLMGW;
	const char *token_file = DEFAULT_TOKEN_FILE;
	char out[ACB_REQ_MAX];
	struct pollfd pfd;
	int afd, i;

	for (i = 1; i < argc; i++) {
		if (!strcmp(argv[i], "--llmgw") && i + 1 < argc)
			base = argv[++i];
		else if (!strcmp(argv[i], "--token-file") && i + 1 < argc)
			token_file = argv[++i];
		else if (!strcmp(argv[i], "-h") || !strcmp(argv[i], "--help")) {
			usage(stdout);
			return 0;
		} else {
			fprintf(stderr, "unknown argument: %s\n\n", argv[i]);
			usage(stderr);
			return 1;
		}
	}
	if (gw_parse_base(base) < 0)
		return 1;
	snprintf(gw.token_file, sizeof(gw.token_file), "%s", token_file);

	signal(SIGPIPE, SIG_IGN);	/* sockets must not kill the agent */

	afd = agent_create(AGENT_NAME, ACB_CLASS_SYS);
	if (afd < 0) {
		perror("agent_create");
		return 1;
	}
	if (agent_attach(afd) < 0) {
		perror("agent_attach");
		return 1;
	}
	for (i = 0; skills[i].name; i++)
		if (agent_add_skill(afd, skills[i].name, skills[i].schema) < 0)
			fprintf(stderr,
				"[sysagent_llm] warning: add_skill %s: %s\n",
				skills[i].name, strerror(errno));

	token_ensure();	/* warm the cache; non-fatal when llmgwd is late */

	fprintf(stderr, "[sysagent_llm] %s serving %d skills -> http://%s:%d\n",
		AGENT_NAME, i, GW_HOST, gw.port);

	pfd.fd = afd;
	pfd.events = POLLIN;
	for (;;) {
		struct acb_recv r;

		if (poll(&pfd, 1, -1) < 0) {
			if (errno == EINTR)
				continue;
			break;
		}
		while (agent_recv_request(afd, &r) == 0) {
			int n = -1;

			for (i = 0; skills[i].name; i++)
				if (!strcmp(skills[i].name, r.skill)) {
					n = skills[i].fn(out, sizeof(out),
							r.buf, r.len,
							r.caller_aid);
					break;
				}
			if (n < 0) {
				char ebuf[ACB_SKILL_MAX * 6 + 8];

				json_escape(ebuf, sizeof(ebuf), r.skill,
					    strnlen(r.skill, ACB_SKILL_MAX));
				n = snprintfc(out, sizeof(out),
					      "{\"error\":{\"type\":"
					      "\"unknown_skill\",\"message\":"
					      "\"%s\"}}", ebuf);
			}
			if (agent_reply(afd, r.req_id, out,
					(__u32)n) < 0)
				fprintf(stderr,
					"[sysagent_llm] reply: %s\n",
					strerror(errno));
		}
		if (pfd.revents & POLLHUP)
			break;
	}
	return 0;
}
#endif /* !SYSAGENT_LLM_NO_MAIN */
