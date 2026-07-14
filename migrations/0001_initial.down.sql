-- Development convenience only; not a guaranteed-safe rollback path once real
-- data exists under a released version. Dropped in reverse dependency order;
-- the vector extension is left in place.
drop table if exists migration_state;
drop table if exists audit_log;
drop table if exists tokens;
drop table if exists links;
drop table if exists chunks;
drop table if exists notes;
drop table if exists embedding_providers;
