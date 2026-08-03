#!/usr/bin/env sh
# Empties slusk's pipeline tables ahead of the pipeline-state-machine
# deploy (see docs/superpowers/specs/2026-07-06-pipeline-state-machine-design.md).
#
# Wipes:  album_jobs, candidate_attempts, candidates, transfers, job_events
# Keeps:  known_users, artist_user_reliability (peer history has forward value)
#
# Jobs self-heal: every still-wanted album reappears as WANTED on the first
# WantedSync tick after the new version starts. Remember to also clear the
# slskd downloads directory (manual step) so leftover files from orphaned
# transfers don't collide with fresh attempts.
#
# Usage:
#   scripts/clean-slate-pipeline.sh "$DSN"
#   DSN=postgres://slusk:...@host:5432/slusk scripts/clean-slate-pipeline.sh
#
# Run while slusk is STOPPED. Requires psql.

set -eu

DSN="${1:-${DSN:-}}"
if [ -z "$DSN" ]; then
    echo "usage: $0 <postgres-dsn>   (or set DSN env var)" >&2
    exit 1
fi

# TRUNCATE each table only if it exists: candidate_attempts is gone after the
# new schema lands, candidates does not exist before it — the script stays
# re-runnable across both sides of the deploy. CASCADE covers the
# transfers→candidates/candidate_attempts and candidates→album_jobs FKs
# regardless of the order tables are visited in.
psql "$DSN" --set ON_ERROR_STOP=1 <<'SQL'
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['transfers', 'candidate_attempts', 'candidates', 'album_jobs', 'job_events'] LOOP
        IF to_regclass(t) IS NOT NULL THEN
            EXECUTE format('TRUNCATE TABLE %I RESTART IDENTITY CASCADE', t);
            RAISE NOTICE 'truncated %', t;
        ELSE
            RAISE NOTICE 'skipped % (does not exist)', t;
        END IF;
    END LOOP;
END
$$;
SQL

echo "done. peer history (known_users, artist_user_reliability) untouched."
