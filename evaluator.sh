#!/bin/sh
set -e
case "$1" in
  dummy)           exec ./dummy-evaluator ;;
  queue-analysis)  exec ./queue-analysis-evaluator ;;
  blis)            exec ./blis-evaluator ;;
  vllm-server)     exec ./vllm-server-evaluator ;;
  *)               echo "Unknown backend: $1. Use: dummy, queue-analysis, blis, vllm-server" >&2; exit 1 ;;
esac
