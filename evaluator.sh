#!/bin/sh
set -e
case "$1" in
  dummy)           exec ./dummy-evaluator ;;
  queue-analysis)  exec ./queue-analysis-evaluator ;;
  blis)            exec ./blis-evaluator ;;
  *)               echo "Unknown backend: $1. Use: dummy, queue-analysis, blis" >&2; exit 1 ;;
esac
