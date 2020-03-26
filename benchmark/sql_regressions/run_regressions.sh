#!/bin/bash

set -eo pipefail

function fail() {
    1>&2 echo "$@"
    exit 1
}

logictest="../../go/libraries/doltcore/sqle/logictest"
logictest_main="$logictest"/main

if [[ "$#" -ne 1 ]]; then
    fail Usage: ./run_regressions.sh ENV_VARIABLES_FILE
fi

source "$1"
if [ -z "$DOLT_CONFIG_PATH" ]; then fail Must supply DOLT_CONFIG_PATH; fi
if [ -z "$DOLT_GLOBAL_CONFIG" ]; then fail Must supply DOLT_GLOBAL_CONFIG; fi
if [ -z "$CREDSDIR" ]; then fail Must supply CREDSDIR; fi
if [ -z "$DOLT_CREDS" ]; then fail Must supply DOLT_CREDS; fi
if [ -z "$CREDS_HASH" ]; then fail Must supply CREDS_HASH; fi
if [ -z "$JOB_TYPE" ]; then fail Must supply DOLT_VERSION; fi
if [ -z "$TEST_N_TIMES" ]; then fail Must supply DOLT_VERSION; fi

if [[ -z "$DOLT_VERSION" ]] && [[ -z "$DOLT_RELEASE" ]]; then
  fail Must supply DOLT_VERSION;
  elif [[ -z "$DOLT_VERSION" && -n "$DOLT_RELEASE" ]]; then
    DOLT_VERSION="$DOLT_RELEASE";
fi

re='^[0-9]+$'
if ! [[ $TEST_N_TIMES =~ $re ]] ; then
   fail TEST_N_TIMES must be a number
fi

function setup() {
    rm -rf "$CREDSDIR"
    mkdir -p "$CREDSDIR"
    cat "$DOLT_CREDS" > "$CREDSDIR"/"$CREDS_HASH".jwk
    echo "$DOLT_GLOBAL_CONFIG" > "$DOLT_CONFIG_PATH"/config_global.json
    dolt config --global --add user.creds "$CREDS_HASH"
    dolt config --global --add metrics.disabled true
    dolt version
    rm -rf temp
    mkdir temp
}

function run_once() {
    test_num="$1"

    local results=temp/results"$test_num".log
    local parsed=temp/parsed"$test_num".json

    rm -rf .dolt
    dolt init
    echo "Running tests and generating $results"
    go run . run ../../../../../../sqllogictest/test/select1.test > "$results"
    echo "Parsing $results and generating $parsed"
    go run . parse "$DOLT_VERSION" temp/results"$test_num".log > "$parsed"
}

function run() {
    seq 1 $TEST_N_TIMES | while read test_num; do
        run_once "$test_num"
    done
    rm -rf .dolt
}

function import_one_nightly() {
    test_num="$1"
    dolt table import -u nightly_dolt_results ../"$logictest_main"/temp/parsed"$test_num".json
    dolt add nightly_dolt_results
    dolt commit -m "update dolt sql performance results ($DOLT_VERSION) ($test_num)"
}

function import_nightly() {
    dolt checkout nightly
    seq 1 $TEST_N_TIMES | while read test_num; do
        import_one_nightly "$test_num"
    done
    dolt sql -r csv -q "\
select version, test_file, line_num, avg(duration) as mean_duration, result from dolt_history_nightly_dolt_results where version=\"${DOLT_VERSION}\" group by line_num;\
" > nightly_mean.csv
    dolt table import -u nightly_dolt_mean_results nightly_mean.csv
    dolt add nightly_dolt_mean_results
    dolt commit -m "update dolt sql performance mean results ($DOLT_VERSION)"
    dolt push origin nightly

    dolt checkout regressions
    dolt merge nightly
    dolt add .
    dolt commit -m "merge nightly"
    dolt push origin regressions

    dolt checkout releases
    dolt sql -r csv -q "\
select * from releases_dolt_mean_results;\
" > releases_mean.csv
    rm -f regressions_db
    touch regressions_db
    sqlite3 regressions_db < ../"$logictest"/regressions.sql
    cp ../"$logictest"/import.sql .
    sqlite3 regressions_db < import.sql
    echo "Checking for test regressions..."

    duration_query_output=`sqlite3 regressions_db 'select * from releases_nightly_duration_change'`
    result_query_output=`sqlite3 regressions_db 'select * from releases_nightly_result_change'`

    duration_regressions=`echo $duration_query_output | sed '/^\s*$/d' | wc -l | tr -d '[:space:]'`
    result_regressions=`echo $result_query_output | sed '/^\s*$/d' | wc -l | tr -d '[:space:]'`

    if [ "$duration_regressions" != 0 ]; then echo "Duration regression found, $duration_regressions != 0" && echo $duration_query_output && exit 1; else echo "No duration regressions found"; fi
    if [ "$result_regressions" != 0 ]; then echo "Result regression found, $result_regressions != 0" && echo $result_query_output && exit 1; else echo "No result regressions found"; fi
}

function rebuild_dolt() {
    echo "Removing dolt version $DOLT_VERSION..."
    rm ../../.ci_bin/dolt
    echo "Rebuilding dolt from current checkout..."
    pushd ../../go && \
    pwd
    go get -mod=readonly ./... && \
    go build -mod=readonly -o ../.ci_bin/dolt ./cmd/dolt/. && \
    popd
    dolt version
}

function import_one_releases() {
    test_num="$1"
    dolt table import -u releases_dolt_results ../"$logictest_main"/temp/parsed"$test_num".json
    dolt add releases_dolt_results
    dolt commit -m "update dolt sql performance results ($DOLT_VERSION) ($test_num)"
}

function import_releases() {
    dolt checkout releases
    seq 1 $TEST_N_TIMES | while read test_num; do
        import_one_releases "$test_num"
    done
    dolt sql -r csv -q "\
select version, test_file, line_num, avg(duration) as mean_duration, result from dolt_history_releases_dolt_results where version=\"${DOLT_VERSION}\" group by line_num;\
" > releases_mean.csv
    dolt table import -u releases_dolt_mean_results releases_mean.csv
    dolt add releases_dolt_mean_results
    dolt commit -m "update dolt sql performance mean results ($DOLT_VERSION)"
    dolt push origin releases

    dolt checkout regressions
    dolt merge releases
    dolt add .
    dolt commit -m "merge releases"
    dolt push origin regressions
}

(cd "$logictest_main" && setup && run)

rm -rf dolt-sql-performance
dolt clone Liquidata/dolt-sql-performance

if [[ "$JOB_TYPE" == "nightly" ]]; then
  (cd dolt-sql-performance && import_nightly);
  elif [ "$JOB_TYPE" == "release" ]; then
      (rebuild_dolt && cd dolt-sql-performance && import_releases)
  else fail Unknown JOB_TYPE specified;
fi
