# sysagent_llm — sys.llm skill carrier for the Kleinia model gateway.
#
# AGENT_USR points at the agent userspace tree (libagent + UAPI headers);
# there is no default — set it explicitly when building the carrier:
#   make AGENT_USR=/path/to/agent-usr
# Dependencies: libc only — libagent.c is compiled in from source, no
# libcurl, no libagenturi (the carrier never issues agent:// calls).
AGENT_USR ?=
CC       ?= cc
CFLAGS   ?= -O2 -g
WARN     := -Wall -Wextra
CPPFLAGS := -I$(AGENT_USR)/include -I$(AGENT_USR)/libagent -I.

BIN  := sysagent_llm
TEST := test_fwd

all: $(BIN)

# the real carrier: links libagent.c from the AGENT_USR tree
$(BIN): sysagent_llm.c fwd.c fwd.h Makefile
	@test -n "$(AGENT_USR)" || { echo 'AGENT_USR not set: point it at the agent userspace tree (include/ + libagent/), e.g. make AGENT_USR=/path/to/agent-usr' >&2; exit 1; }
	@test -f "$(AGENT_USR)/libagent/libagent.c" || { echo "libagent.c not found under AGENT_USR=$(AGENT_USR)" >&2; exit 1; }
	$(CC) $(CFLAGS) $(WARN) $(CPPFLAGS) -o $@ sysagent_llm.c fwd.c \
	      $(AGENT_USR)/libagent/libagent.c

# offline unit tests: sysagent_llm.c is #included with main() compiled
# out, so no kernel/libagent dependency; only fwd.c is linked.
$(TEST): test_fwd.c sysagent_llm.c fwd.c fwd.h Makefile
	$(CC) $(CFLAGS) $(WARN) $(CPPFLAGS) -o $@ test_fwd.c fwd.c

test: $(TEST)
	./$(TEST)

# semi-integration vs a real llmgwd (builds it if bin/llmgwd is missing)
e2e: $(TEST)
	./test_e2e.sh

clean:
	rm -f $(BIN) $(TEST)

.PHONY: all test e2e clean
