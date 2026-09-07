-- TogetherGo — database and role bootstrap.
--
-- Runs once, on first initialisation of an empty data directory, as the
-- superuser defined by POSTGRES_USER. If you change this file you must
-- recreate the volume (`make clean`) for it to take effect again.
--
-- Architecture rule: database per service. Four databases, four roles, each
-- role owning exactly one database and able to connect to exactly one
-- database. Cross-service database access must fail loudly at connection
-- time rather than silently working, so a stray DSN in a config file becomes
-- an immediate error instead of a hidden coupling.

\set ON_ERROR_STOP on

-- Passwords come from the container environment (see docker-compose.yml),
-- never hard-coded here.
\getenv identity_password IDENTITY_DB_PASSWORD
\getenv trip_password TRIP_DB_PASSWORD
\getenv chat_password CHAT_DB_PASSWORD
\getenv notification_password NOTIFICATION_DB_PASSWORD

-- ---------------------------------------------------------------------------
-- Roles
-- ---------------------------------------------------------------------------

CREATE ROLE identity_user     LOGIN PASSWORD :'identity_password';
CREATE ROLE trip_user         LOGIN PASSWORD :'trip_password';
CREATE ROLE chat_user         LOGIN PASSWORD :'chat_password';
CREATE ROLE notification_user LOGIN PASSWORD :'notification_password';

-- An AWS RDS master user has CREATEROLE but is not a true PostgreSQL
-- superuser. PostgreSQL therefore requires it to be a member of a role before
-- it can create a database owned by that role. This is harmless for the local
-- container superuser and makes the same bootstrap usable on RDS.
SELECT format('GRANT identity_user TO %I', current_user) \gexec
SELECT format('GRANT trip_user TO %I', current_user) \gexec
SELECT format('GRANT chat_user TO %I', current_user) \gexec
SELECT format('GRANT notification_user TO %I', current_user) \gexec

-- ---------------------------------------------------------------------------
-- Databases
-- ---------------------------------------------------------------------------

CREATE DATABASE identity_db     OWNER identity_user;
CREATE DATABASE trip_db         OWNER trip_user;
CREATE DATABASE chat_db         OWNER chat_user;
CREATE DATABASE notification_db OWNER notification_user;

-- ---------------------------------------------------------------------------
-- Connection privileges
--
-- Postgres grants CONNECT and TEMP to PUBLIC on every new database. Revoking
-- from PUBLIC is what actually closes the door; the explicit REVOKEs for the
-- other three roles are belt-and-braces and document the intent in one place.
-- The owning role keeps its privileges as database owner, and is granted
-- CONNECT explicitly so the ACL reads unambiguously.
-- ---------------------------------------------------------------------------

REVOKE ALL   ON DATABASE identity_db     FROM PUBLIC;
REVOKE ALL   ON DATABASE trip_db         FROM PUBLIC;
REVOKE ALL   ON DATABASE chat_db         FROM PUBLIC;
REVOKE ALL   ON DATABASE notification_db FROM PUBLIC;

REVOKE CONNECT ON DATABASE identity_db     FROM trip_user, chat_user, notification_user;
REVOKE CONNECT ON DATABASE trip_db         FROM identity_user, chat_user, notification_user;
REVOKE CONNECT ON DATABASE chat_db         FROM identity_user, trip_user, notification_user;
REVOKE CONNECT ON DATABASE notification_db FROM identity_user, trip_user, chat_user;

GRANT CONNECT, TEMPORARY ON DATABASE identity_db     TO identity_user;
GRANT CONNECT, TEMPORARY ON DATABASE trip_db         TO trip_user;
GRANT CONNECT, TEMPORARY ON DATABASE chat_db         TO chat_user;
GRANT CONNECT, TEMPORARY ON DATABASE notification_db TO notification_user;

-- ---------------------------------------------------------------------------
-- Per-database schema setup
--
-- From Postgres 15 on, the public schema is owned by pg_database_owner and
-- PUBLIC has no CREATE on it. Making the service role the explicit owner of
-- public keeps migrations (goose / alembic) working without superuser.
-- ---------------------------------------------------------------------------

\connect identity_db
ALTER SCHEMA public OWNER TO identity_user;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT ALL ON SCHEMA public TO identity_user;

\connect chat_db
ALTER SCHEMA public OWNER TO chat_user;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT ALL ON SCHEMA public TO chat_user;

\connect notification_db
ALTER SCHEMA public OWNER TO notification_user;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT ALL ON SCHEMA public TO notification_user;

-- trip_db is the only database that stores geometry (trip routes, SRID 4326
-- geography), so it is the only one that gets PostGIS.
\connect trip_db
ALTER SCHEMA public OWNER TO trip_user;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT ALL ON SCHEMA public TO trip_user;

CREATE EXTENSION IF NOT EXISTS postgis;

-- spatial_ref_sys is created by the extension and owned by the superuser;
-- trip_user needs to read it for coordinate transforms.
GRANT SELECT ON TABLE public.spatial_ref_sys TO trip_user;
