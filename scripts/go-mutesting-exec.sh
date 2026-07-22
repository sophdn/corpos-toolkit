#!/bin/bash
# scripts/go-mutesting-exec.sh — custom go-mutesting exec command for this repo.
#
# go-mutesting (avito-tech fork) has NO native build-tag support, and this
# module REQUIRES `-tags sqlite_fts5` to compile (the CGO SQLite/FTS5 driver).
# Its built-in exec runs a bare `go test`, which fails to build refresolve and
# every other package that touches the DB. This wrapper is the avito default
# exec script (scripts/exec/test-mutated-package.sh) with `-tags sqlite_fts5`
# added to the `go test` invocation — nothing else changed.
#
# Usage (from the go/ module dir):
#   MUTATE_TIMEOUT=120 go-mutesting \
#     --exec="$(git rev-parse --show-toplevel)/scripts/go-mutesting-exec.sh" \
#     --exec-timeout=180 internal/refresolve/discipline_intent.go
#
# Mutation testing is an on-demand analysis tool (NOT in the precommit gate —
# it re-runs the suite per mutant, far too slow for every commit). See skill
# dependency-vetting-discipline for how go-mutesting was vetted + pinned.
#
# IN-PLACE MUTATION + HARD-KILL RECOVERY: this wrapper moves the target file
# to <file>.tmp, copies the mutant over the real path, runs the suite, then
# restores via clean_up (and the SIGHUP/SIGINT/SIGTERM trap below). A SIGKILL
# (OOM, kill -9, terminal close) is UNTRAPPABLE — it strands the applied mutant
# in <file> with the pristine original left in <file>.tmp. This reads like
# uncommitted source corruption; it is not. Recover with:
#     git checkout <file> && rm <file>.tmp   # <file>.tmp is byte-identical to HEAD
#     git checkout go/go.sum                  # revert the tool's dep pollution
# `make -C go mutest` has a pre-run guard that refuses to start while a stranded
# *.go.tmp exists under PKG, so a mutant can't silently compound across runs.

if [ -z ${MUTATE_CHANGED+x} ]; then echo "MUTATE_CHANGED is not set"; exit 1; fi
if [ -z ${MUTATE_ORIGINAL+x} ]; then echo "MUTATE_ORIGINAL is not set"; exit 1; fi
if [ -z ${MUTATE_PACKAGE+x} ]; then echo "MUTATE_PACKAGE is not set"; exit 1; fi

function clean_up {
	if [ -f $MUTATE_ORIGINAL.tmp ];
	then
		mv $MUTATE_ORIGINAL.tmp $MUTATE_ORIGINAL
	fi
}

function sig_handler {
	clean_up

	exit $GOMUTESTING_RESULT
}
trap sig_handler SIGHUP SIGINT SIGTERM

export GOMUTESTING_DIFF=$(diff -u $MUTATE_ORIGINAL $MUTATE_CHANGED)

mv $MUTATE_ORIGINAL $MUTATE_ORIGINAL.tmp
cp $MUTATE_CHANGED $MUTATE_ORIGINAL

export MUTATE_TIMEOUT=${MUTATE_TIMEOUT:-10}

if [ -n "$TEST_RECURSIVE" ]; then
	TEST_RECURSIVE="/..."
fi

# The ONLY change from the avito default exec: -tags sqlite_fts5.
GOMUTESTING_TEST=$(go test -tags sqlite_fts5 -timeout $(printf '%ds' $MUTATE_TIMEOUT) $MUTATE_PACKAGE$TEST_RECURSIVE 2>&1)
export GOMUTESTING_RESULT=$?

if [ "$MUTATE_DEBUG" = true ] ; then
	echo "$GOMUTESTING_TEST"
fi

clean_up

case $GOMUTESTING_RESULT in
0) # tests passed -> FAIL (mutant survived)
	echo "$GOMUTESTING_DIFF"

	exit 1
	;;
1) # tests failed -> PASS (mutant killed)
	if [ "$MUTATE_DEBUG" = true ] ; then
		echo "$GOMUTESTING_DIFF"
	fi

	exit 0
	;;
2) # did not compile -> SKIP
	if [ "$MUTATE_VERBOSE" = true ] ; then
		echo "Mutation did not compile"
	fi

	if [ "$MUTATE_DEBUG" = true ] ; then
		echo "$GOMUTESTING_DIFF"
	fi

	exit 2
	;;
*) # Unknown exit code -> SKIP
	echo "Unknown exit code"
	echo "$GOMUTESTING_DIFF"

	exit $GOMUTESTING_RESULT
	;;
esac
