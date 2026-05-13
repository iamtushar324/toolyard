-- mart-views.sql — derived views over the migrated dim/fact tables.
--
-- These are the ClickHouse-dialect translations of the views that
-- lived in DuckDB pre-cutover (see git history of mart_views_to_port.sql).
-- Re-applied idempotently by deploy/install.sh on every install. Safe to
-- run by hand too:
--
--   PW=$(sudo cat /var/lib/toolyard/clickhouse-runtime.env | cut -d= -f2)
--   curl -sf -u "default:$PW" --data-binary @deploy/clickhouse/mart-views.sql \
--        http://127.0.0.1:18123/?multi_statements=1
--
-- CH treats CREATE OR REPLACE VIEW as a metadata-only operation, so
-- re-running won't impact query performance or block readers.

CREATE OR REPLACE VIEW mart.investments_history AS
SELECT as_of_date,
       sum(current_value)   AS total_value,
       sum(unrealized_pnl)  AS unrealized_pnl
  FROM raw.investment_valuations
 GROUP BY as_of_date
 ORDER BY as_of_date;

CREATE OR REPLACE VIEW mart.v_exercise_set_effective AS
SELECT s.exercise_set_key,
       s.workout_key,
       s.exercise_key,
       s.event_date,
       s.set_index,
       s.exercise_order,
       s.set_type,
       s.weight_kg,
       s.reps,
       s.duration_seconds,
       s.distance_m,
       s.rir,
       s.rpe,
       s.was_failure,
       s.estimated_1rm_kg,
       s.total_volume_kg,
       e.is_unilateral,
       e.primary_muscle_group,
       e.movement_pattern,
       CASE WHEN e.is_unilateral
            THEN s.total_volume_kg * 2
            ELSE s.total_volume_kg
       END AS effective_volume_kg
  FROM mart.fact_exercise_set AS s
 INNER JOIN mart.dim_exercise AS e
    ON e.exercise_key = s.exercise_key;
