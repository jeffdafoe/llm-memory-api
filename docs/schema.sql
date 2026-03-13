--
-- PostgreSQL database dump
--

-- Dumped from database version 17.8 (Debian 17.8-0+deb13u1)
-- Dumped by pg_dump version 17.8 (Debian 17.8-0+deb13u1)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: vector; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: actors; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.actors (
    id integer NOT NULL,
    name character varying(50) NOT NULL,
    type character varying(20) DEFAULT 'agent'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT actors_type_check CHECK (((type)::text = ANY ((ARRAY['agent'::character varying, 'user'::character varying, 'system'::character varying])::text[])))
);


--
-- Name: actors_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.actors_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: actors_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.actors_id_seq OWNED BY public.actors.id;


--
-- Name: agent_api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agent_api_keys (
    id integer NOT NULL,
    key_hash character varying(255) NOT NULL,
    key_salt character varying(64) NOT NULL,
    label character varying(100),
    created_at timestamp with time zone DEFAULT now(),
    last_used_at timestamp with time zone,
    revoked_at timestamp with time zone,
    actor_id integer NOT NULL
);


--
-- Name: agent_api_keys_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.agent_api_keys_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: agent_api_keys_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.agent_api_keys_id_seq OWNED BY public.agent_api_keys.id;


--
-- Name: agent_permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agent_permissions (
    permission_id integer NOT NULL,
    granted_at timestamp with time zone DEFAULT now(),
    actor_id integer NOT NULL
);


--
-- Name: agent_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agent_sessions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    token_hash text NOT NULL,
    token_salt text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    subsystem text,
    actor_id integer NOT NULL
);


--
-- Name: agents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agents (
    registered_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen timestamp with time zone,
    token_hash character varying(128),
    token_salt character varying(64),
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    passphrase_rotated_at timestamp with time zone,
    startup_instructions text,
    expertise text DEFAULT '[]'::text NOT NULL,
    provider character varying(50),
    model character varying(100),
    virtual boolean DEFAULT false NOT NULL,
    personality text,
    api_key character varying(512),
    configuration text,
    active_since timestamp with time zone,
    cache_prompts boolean DEFAULT false NOT NULL,
    learning_enabled boolean DEFAULT true NOT NULL,
    max_tokens integer,
    temperature numeric,
    cost_budget_daily numeric(10,2),
    cost_budget_monthly numeric(10,2),
    actor_id integer NOT NULL,
    CONSTRAINT chk_agents_provider CHECK (((provider IS NULL) OR ((provider)::text = ANY ((ARRAY['anthropic'::character varying, 'google'::character varying, 'openai'::character varying, 'perplexity'::character varying])::text[])))),
    CONSTRAINT chk_agents_status CHECK (((status)::text = 'active'::text))
);


--
-- Name: agent_status; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.agent_status AS
 SELECT ac.id AS actor_id,
    ac.name AS agent,
    ac.type AS actor_type,
        CASE
            WHEN (a.virtual = true) THEN 'available'::text
            WHEN (a.last_seen > (now() - '00:15:00'::interval)) THEN 'online'::text
            WHEN (a.last_seen IS NOT NULL) THEN 'offline'::text
            ELSE 'unknown'::text
        END AS status,
    a.last_seen,
    a.passphrase_rotated_at,
    ac.created_at AS registered_at,
    a.expertise,
    a.provider,
    a.model,
    a.virtual,
    a.personality,
    a.cost_budget_daily,
    a.cost_budget_monthly,
        CASE
            WHEN ((a.active_since IS NOT NULL) AND (a.active_since > (now() - '00:30:00'::interval))) THEN a.active_since
            ELSE NULL::timestamp with time zone
        END AS active_since,
    a.cache_prompts,
    a.learning_enabled,
    a.max_tokens,
    a.temperature
   FROM (public.actors ac
     JOIN public.agents a ON ((a.actor_id = ac.id)));


--
-- Name: chat_messages; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.chat_messages (
    id integer NOT NULL,
    message text NOT NULL,
    sent_at timestamp with time zone DEFAULT now() NOT NULL,
    acked_at timestamp with time zone,
    channel character varying(50) DEFAULT NULL::character varying,
    from_actor_id integer NOT NULL,
    to_actor_id integer
);


--
-- Name: chat_messages_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.chat_messages_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: chat_messages_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.chat_messages_id_seq OWNED BY public.chat_messages.id;


--
-- Name: memory_chunks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.memory_chunks (
    id integer NOT NULL,
    namespace character varying(50) NOT NULL,
    source_file character varying(500) NOT NULL,
    heading character varying(500),
    chunk_text text NOT NULL,
    embedding public.vector(1536),
    ingested_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: chunks_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.chunks_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: chunks_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.chunks_id_seq OWNED BY public.memory_chunks.id;


--
-- Name: config; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.config (
    key character varying(100) NOT NULL,
    value text NOT NULL,
    description text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: discussion_ballots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.discussion_ballots (
    vote_id integer NOT NULL,
    choice integer NOT NULL,
    reason text,
    cast_at timestamp with time zone DEFAULT now() NOT NULL,
    actor_id integer NOT NULL
);


--
-- Name: discussion_participants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.discussion_participants (
    discussion_id integer NOT NULL,
    status character varying(20) DEFAULT 'invited'::character varying NOT NULL,
    invited_at timestamp with time zone DEFAULT now() NOT NULL,
    joined_at timestamp with time zone,
    role character varying(20) DEFAULT 'required'::character varying NOT NULL,
    deferred_at timestamp with time zone,
    defer_count integer DEFAULT 0,
    actor_id integer NOT NULL,
    CONSTRAINT chk_discussion_participants_role CHECK (((role)::text = ANY ((ARRAY['required'::character varying, 'optional'::character varying])::text[]))),
    CONSTRAINT chk_discussion_participants_status CHECK (((status)::text = ANY ((ARRAY['invited'::character varying, 'joined'::character varying, 'left'::character varying, 'timed_out'::character varying])::text[]))),
    CONSTRAINT discussion_participants_status_check CHECK (((status)::text = ANY ((ARRAY['invited'::character varying, 'joined'::character varying, 'left'::character varying, 'timed_out'::character varying, 'deferred'::character varying])::text[])))
);


--
-- Name: discussion_votes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.discussion_votes (
    id integer NOT NULL,
    discussion_id integer NOT NULL,
    question text NOT NULL,
    status character varying(20) DEFAULT 'open'::character varying NOT NULL,
    type character varying(20) DEFAULT 'general'::character varying NOT NULL,
    threshold character varying(20) DEFAULT 'unanimous'::character varying NOT NULL,
    closes_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    closed_at timestamp with time zone,
    proposed_by_actor_id integer NOT NULL,
    CONSTRAINT chk_discussion_votes_status CHECK (((status)::text = ANY ((ARRAY['open'::character varying, 'closed'::character varying])::text[]))),
    CONSTRAINT chk_discussion_votes_threshold CHECK (((threshold)::text = ANY ((ARRAY['unanimous'::character varying, 'majority'::character varying])::text[]))),
    CONSTRAINT chk_discussion_votes_type CHECK (((type)::text = ANY ((ARRAY['general'::character varying, 'conclude'::character varying])::text[])))
);


--
-- Name: discussion_votes_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.discussion_votes_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: discussion_votes_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.discussion_votes_id_seq OWNED BY public.discussion_votes.id;


--
-- Name: discussions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.discussions (
    id integer NOT NULL,
    topic text NOT NULL,
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    channel character varying(50),
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    concluded_at timestamp with time zone,
    mode character varying(20) DEFAULT 'realtime'::character varying NOT NULL,
    context text,
    timeout_at timestamp with time zone,
    outcome character varying(20),
    created_by_actor_id integer NOT NULL,
    CONSTRAINT chk_discussions_mode CHECK (((mode)::text = ANY ((ARRAY['realtime'::character varying, 'async'::character varying])::text[]))),
    CONSTRAINT chk_discussions_outcome CHECK (((outcome)::text = ANY ((ARRAY['consensus'::character varying, 'deadlock'::character varying, 'partial'::character varying, 'abandoned'::character varying])::text[]))),
    CONSTRAINT chk_discussions_status CHECK (((status)::text = ANY ((ARRAY['waiting'::character varying, 'active'::character varying, 'concluded'::character varying, 'cancelled'::character varying, 'timed_out'::character varying])::text[])))
);


--
-- Name: discussions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.discussions_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: discussions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.discussions_id_seq OWNED BY public.discussions.id;


--
-- Name: documents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.documents (
    id integer NOT NULL,
    namespace character varying(64) NOT NULL,
    slug character varying(255) NOT NULL,
    title character varying(500),
    content text NOT NULL,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now(),
    deleted_at timestamp with time zone,
    created_by_actor_id integer
);


--
-- Name: documents_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.documents_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: documents_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.documents_id_seq OWNED BY public.documents.id;


--
-- Name: error_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.error_log (
    id integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    subsystem text NOT NULL,
    action text NOT NULL,
    context text,
    context_id text,
    error_message text NOT NULL,
    error_detail text,
    actor_id integer
);


--
-- Name: error_log_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.error_log_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: error_log_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.error_log_id_seq OWNED BY public.error_log.id;


--
-- Name: mail; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mail (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    subject character varying(500) NOT NULL,
    body text NOT NULL,
    sent_at timestamp with time zone DEFAULT now() NOT NULL,
    acked_at timestamp with time zone,
    deleted_at timestamp with time zone,
    from_actor_id integer,
    to_actor_id integer NOT NULL
);


--
-- Name: mcp_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mcp_sessions (
    session_id text NOT NULL,
    tools_hash text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    actor_id integer NOT NULL
);


--
-- Name: migrations_applied; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.migrations_applied (
    migration_id character varying(50) NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: namespace_permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.namespace_permissions (
    id integer NOT NULL,
    actor_id integer NOT NULL,
    namespace character varying(100) NOT NULL,
    can_read boolean DEFAULT false NOT NULL,
    can_write boolean DEFAULT false NOT NULL,
    can_delete boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: namespace_permissions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.namespace_permissions_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: namespace_permissions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.namespace_permissions_id_seq OWNED BY public.namespace_permissions.id;


--
-- Name: permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.permissions (
    id integer NOT NULL,
    name character varying(100) NOT NULL,
    created_at timestamp with time zone DEFAULT now()
);


--
-- Name: permissions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.permissions_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: permissions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.permissions_id_seq OWNED BY public.permissions.id;


--
-- Name: request_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.request_log (
    id integer NOT NULL,
    "timestamp" timestamp with time zone DEFAULT now() NOT NULL,
    method character varying(10) NOT NULL,
    path text,
    status integer,
    duration_ms integer,
    ip character varying(45),
    request_length integer,
    response_length integer,
    actor_id integer
);


--
-- Name: request_log_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.request_log_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: request_log_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.request_log_id_seq OWNED BY public.request_log.id;


--
-- Name: system_errors; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.system_errors (
    id integer NOT NULL,
    source text NOT NULL,
    error_code text NOT NULL,
    context jsonb,
    status text DEFAULT 'unhandled'::text NOT NULL,
    handler_action text,
    resolved_at timestamp with time zone,
    reported_at timestamp with time zone DEFAULT now(),
    actor_id integer
);


--
-- Name: system_errors_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.system_errors_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: system_errors_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.system_errors_id_seq OWNED BY public.system_errors.id;


--
-- Name: templates; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.templates (
    id integer NOT NULL,
    name character varying(100) NOT NULL,
    description text,
    content text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    kind character varying(50) DEFAULT 'welcome'::character varying NOT NULL,
    CONSTRAINT templates_kind_check CHECK (((kind)::text = 'welcome'::text))
);


--
-- Name: user_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_sessions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id integer NOT NULL,
    session_token text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id integer NOT NULL,
    username text NOT NULL,
    password_hash text NOT NULL,
    password_salt text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_login timestamp with time zone
);


--
-- Name: users_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.users_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: users_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.users_id_seq OWNED BY public.users.id;


--
-- Name: virtual_agent_usage; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.virtual_agent_usage (
    id integer NOT NULL,
    provider character varying(50),
    model character varying(100),
    input_tokens integer DEFAULT 0,
    output_tokens integer DEFAULT 0,
    cache_write_tokens integer DEFAULT 0,
    cache_read_tokens integer DEFAULT 0,
    cost numeric(10,6) DEFAULT 0 NOT NULL,
    context character varying(50),
    created_at timestamp with time zone DEFAULT now(),
    actor_id integer NOT NULL
);


--
-- Name: virtual_agent_usage_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.virtual_agent_usage_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: virtual_agent_usage_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.virtual_agent_usage_id_seq OWNED BY public.virtual_agent_usage.id;


--
-- Name: welcome_templates_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.welcome_templates_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: welcome_templates_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.welcome_templates_id_seq OWNED BY public.templates.id;


--
-- Name: actors id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.actors ALTER COLUMN id SET DEFAULT nextval('public.actors_id_seq'::regclass);


--
-- Name: agent_api_keys id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_api_keys ALTER COLUMN id SET DEFAULT nextval('public.agent_api_keys_id_seq'::regclass);


--
-- Name: chat_messages id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_messages ALTER COLUMN id SET DEFAULT nextval('public.chat_messages_id_seq'::regclass);


--
-- Name: discussion_votes id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_votes ALTER COLUMN id SET DEFAULT nextval('public.discussion_votes_id_seq'::regclass);


--
-- Name: discussions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussions ALTER COLUMN id SET DEFAULT nextval('public.discussions_id_seq'::regclass);


--
-- Name: documents id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.documents ALTER COLUMN id SET DEFAULT nextval('public.documents_id_seq'::regclass);


--
-- Name: error_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.error_log ALTER COLUMN id SET DEFAULT nextval('public.error_log_id_seq'::regclass);


--
-- Name: memory_chunks id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memory_chunks ALTER COLUMN id SET DEFAULT nextval('public.chunks_id_seq'::regclass);


--
-- Name: namespace_permissions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespace_permissions ALTER COLUMN id SET DEFAULT nextval('public.namespace_permissions_id_seq'::regclass);


--
-- Name: permissions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.permissions ALTER COLUMN id SET DEFAULT nextval('public.permissions_id_seq'::regclass);


--
-- Name: request_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.request_log ALTER COLUMN id SET DEFAULT nextval('public.request_log_id_seq'::regclass);


--
-- Name: system_errors id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.system_errors ALTER COLUMN id SET DEFAULT nextval('public.system_errors_id_seq'::regclass);


--
-- Name: templates id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.templates ALTER COLUMN id SET DEFAULT nextval('public.welcome_templates_id_seq'::regclass);


--
-- Name: users id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users ALTER COLUMN id SET DEFAULT nextval('public.users_id_seq'::regclass);


--
-- Name: virtual_agent_usage id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.virtual_agent_usage ALTER COLUMN id SET DEFAULT nextval('public.virtual_agent_usage_id_seq'::regclass);


--
-- Name: actors actors_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.actors
    ADD CONSTRAINT actors_name_key UNIQUE (name);


--
-- Name: actors actors_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.actors
    ADD CONSTRAINT actors_pkey PRIMARY KEY (id);


--
-- Name: agent_api_keys agent_api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_api_keys
    ADD CONSTRAINT agent_api_keys_pkey PRIMARY KEY (id);


--
-- Name: agent_permissions agent_permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_permissions
    ADD CONSTRAINT agent_permissions_pkey PRIMARY KEY (actor_id, permission_id);


--
-- Name: agent_sessions agent_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_sessions
    ADD CONSTRAINT agent_sessions_pkey PRIMARY KEY (id);


--
-- Name: agents agents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT agents_pkey PRIMARY KEY (actor_id);


--
-- Name: chat_messages chat_messages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_messages
    ADD CONSTRAINT chat_messages_pkey PRIMARY KEY (id);


--
-- Name: memory_chunks chunks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memory_chunks
    ADD CONSTRAINT chunks_pkey PRIMARY KEY (id);


--
-- Name: config config_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config
    ADD CONSTRAINT config_pkey PRIMARY KEY (key);


--
-- Name: discussion_ballots discussion_ballots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_ballots
    ADD CONSTRAINT discussion_ballots_pkey PRIMARY KEY (vote_id, actor_id);


--
-- Name: discussion_participants discussion_participants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_participants
    ADD CONSTRAINT discussion_participants_pkey PRIMARY KEY (discussion_id, actor_id);


--
-- Name: discussion_votes discussion_votes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_votes
    ADD CONSTRAINT discussion_votes_pkey PRIMARY KEY (id);


--
-- Name: discussions discussions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussions
    ADD CONSTRAINT discussions_pkey PRIMARY KEY (id);


--
-- Name: documents documents_namespace_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.documents
    ADD CONSTRAINT documents_namespace_slug_key UNIQUE (namespace, slug);


--
-- Name: documents documents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.documents
    ADD CONSTRAINT documents_pkey PRIMARY KEY (id);


--
-- Name: error_log error_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.error_log
    ADD CONSTRAINT error_log_pkey PRIMARY KEY (id);


--
-- Name: mail mail_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mail
    ADD CONSTRAINT mail_pkey PRIMARY KEY (id);


--
-- Name: mcp_sessions mcp_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_sessions
    ADD CONSTRAINT mcp_sessions_pkey PRIMARY KEY (session_id);


--
-- Name: migrations_applied migrations_applied_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.migrations_applied
    ADD CONSTRAINT migrations_applied_pkey PRIMARY KEY (migration_id);


--
-- Name: namespace_permissions namespace_permissions_actor_id_namespace_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespace_permissions
    ADD CONSTRAINT namespace_permissions_actor_id_namespace_key UNIQUE (actor_id, namespace);


--
-- Name: namespace_permissions namespace_permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespace_permissions
    ADD CONSTRAINT namespace_permissions_pkey PRIMARY KEY (id);


--
-- Name: permissions permissions_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.permissions
    ADD CONSTRAINT permissions_name_key UNIQUE (name);


--
-- Name: permissions permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.permissions
    ADD CONSTRAINT permissions_pkey PRIMARY KEY (id);


--
-- Name: request_log request_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.request_log
    ADD CONSTRAINT request_log_pkey PRIMARY KEY (id);


--
-- Name: system_errors system_errors_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.system_errors
    ADD CONSTRAINT system_errors_pkey PRIMARY KEY (id);


--
-- Name: agents uq_agents_actor_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT uq_agents_actor_id UNIQUE (actor_id);


--
-- Name: user_sessions user_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_sessions
    ADD CONSTRAINT user_sessions_pkey PRIMARY KEY (id);


--
-- Name: user_sessions user_sessions_session_token_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_sessions
    ADD CONSTRAINT user_sessions_session_token_key UNIQUE (session_token);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: users users_username_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_username_key UNIQUE (username);


--
-- Name: virtual_agent_usage virtual_agent_usage_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.virtual_agent_usage
    ADD CONSTRAINT virtual_agent_usage_pkey PRIMARY KEY (id);


--
-- Name: templates welcome_templates_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.templates
    ADD CONSTRAINT welcome_templates_name_key UNIQUE (name);


--
-- Name: templates welcome_templates_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.templates
    ADD CONSTRAINT welcome_templates_pkey PRIMARY KEY (id);


--
-- Name: idx_agent_api_keys_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_agent_api_keys_actor ON public.agent_api_keys USING btree (actor_id);


--
-- Name: idx_agent_sessions_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_agent_sessions_actor ON public.agent_sessions USING btree (actor_id);


--
-- Name: idx_agent_sessions_expires; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_agent_sessions_expires ON public.agent_sessions USING btree (expires_at);


--
-- Name: idx_chat_messages_to_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_chat_messages_to_actor ON public.chat_messages USING btree (to_actor_id, id);


--
-- Name: idx_chat_messages_unacked; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_chat_messages_unacked ON public.chat_messages USING btree (to_actor_id, channel) WHERE (acked_at IS NULL);


--
-- Name: idx_chunks_embedding; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_chunks_embedding ON public.memory_chunks USING ivfflat (embedding public.vector_cosine_ops) WITH (lists='100');


--
-- Name: idx_chunks_namespace_source; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_chunks_namespace_source ON public.memory_chunks USING btree (namespace, source_file);


--
-- Name: idx_discussion_participants_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_discussion_participants_actor ON public.discussion_participants USING btree (actor_id, status);


--
-- Name: idx_discussion_votes_discussion; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_discussion_votes_discussion ON public.discussion_votes USING btree (discussion_id, status);


--
-- Name: idx_discussions_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_discussions_status ON public.discussions USING btree (status);


--
-- Name: idx_documents_namespace; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_documents_namespace ON public.documents USING btree (namespace);


--
-- Name: idx_error_log_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_error_log_actor ON public.error_log USING btree (actor_id) WHERE (actor_id IS NOT NULL);


--
-- Name: idx_error_log_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_error_log_created ON public.error_log USING btree (created_at DESC);


--
-- Name: idx_error_log_subsystem; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_error_log_subsystem ON public.error_log USING btree (subsystem);


--
-- Name: idx_mail_to_actor_acked; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_mail_to_actor_acked ON public.mail USING btree (to_actor_id, acked_at);


--
-- Name: idx_mcp_sessions_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_mcp_sessions_actor ON public.mcp_sessions USING btree (actor_id);


--
-- Name: idx_ns_perm_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ns_perm_actor ON public.namespace_permissions USING btree (actor_id);


--
-- Name: idx_ns_perm_namespace; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ns_perm_namespace ON public.namespace_permissions USING btree (namespace);


--
-- Name: idx_request_log_timestamp; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_request_log_timestamp ON public.request_log USING btree ("timestamp");


--
-- Name: idx_system_errors_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_system_errors_actor ON public.system_errors USING btree (actor_id) WHERE (actor_id IS NOT NULL);


--
-- Name: idx_system_errors_error_code; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_system_errors_error_code ON public.system_errors USING btree (error_code);


--
-- Name: idx_system_errors_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_system_errors_status ON public.system_errors USING btree (status);


--
-- Name: idx_user_sessions_expires; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_sessions_expires ON public.user_sessions USING btree (expires_at);


--
-- Name: idx_user_sessions_token; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_sessions_token ON public.user_sessions USING btree (session_token);


--
-- Name: idx_user_sessions_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_sessions_user_id ON public.user_sessions USING btree (user_id);


--
-- Name: idx_va_usage_actor_date; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_va_usage_actor_date ON public.virtual_agent_usage USING btree (actor_id, created_at);


--
-- Name: agent_permissions agent_permissions_permission_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_permissions
    ADD CONSTRAINT agent_permissions_permission_id_fkey FOREIGN KEY (permission_id) REFERENCES public.permissions(id);


--
-- Name: discussion_ballots discussion_ballots_vote_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_ballots
    ADD CONSTRAINT discussion_ballots_vote_id_fkey FOREIGN KEY (vote_id) REFERENCES public.discussion_votes(id);


--
-- Name: discussion_participants discussion_participants_discussion_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_participants
    ADD CONSTRAINT discussion_participants_discussion_id_fkey FOREIGN KEY (discussion_id) REFERENCES public.discussions(id);


--
-- Name: discussion_votes discussion_votes_discussion_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_votes
    ADD CONSTRAINT discussion_votes_discussion_id_fkey FOREIGN KEY (discussion_id) REFERENCES public.discussions(id);


--
-- Name: agent_api_keys fk_aak_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_api_keys
    ADD CONSTRAINT fk_aak_actor FOREIGN KEY (actor_id) REFERENCES public.actors(id);


--
-- Name: agents fk_agents_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT fk_agents_actor FOREIGN KEY (actor_id) REFERENCES public.actors(id);


--
-- Name: agent_permissions fk_ap_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_permissions
    ADD CONSTRAINT fk_ap_actor FOREIGN KEY (actor_id) REFERENCES public.actors(id);


--
-- Name: agent_sessions fk_as_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_sessions
    ADD CONSTRAINT fk_as_actor FOREIGN KEY (actor_id) REFERENCES public.actors(id);


--
-- Name: chat_messages fk_chat_from_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_messages
    ADD CONSTRAINT fk_chat_from_actor FOREIGN KEY (from_actor_id) REFERENCES public.actors(id);


--
-- Name: chat_messages fk_chat_to_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_messages
    ADD CONSTRAINT fk_chat_to_actor FOREIGN KEY (to_actor_id) REFERENCES public.actors(id);


--
-- Name: discussion_ballots fk_db_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_ballots
    ADD CONSTRAINT fk_db_actor FOREIGN KEY (actor_id) REFERENCES public.actors(id);


--
-- Name: discussions fk_discussions_created_by; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussions
    ADD CONSTRAINT fk_discussions_created_by FOREIGN KEY (created_by_actor_id) REFERENCES public.actors(id);


--
-- Name: documents fk_docs_created_by; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.documents
    ADD CONSTRAINT fk_docs_created_by FOREIGN KEY (created_by_actor_id) REFERENCES public.actors(id);


--
-- Name: discussion_participants fk_dp_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_participants
    ADD CONSTRAINT fk_dp_actor FOREIGN KEY (actor_id) REFERENCES public.actors(id);


--
-- Name: discussion_votes fk_dv_proposed_by; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.discussion_votes
    ADD CONSTRAINT fk_dv_proposed_by FOREIGN KEY (proposed_by_actor_id) REFERENCES public.actors(id);


--
-- Name: mail fk_mail_from_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mail
    ADD CONSTRAINT fk_mail_from_actor FOREIGN KEY (from_actor_id) REFERENCES public.actors(id);


--
-- Name: mail fk_mail_to_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mail
    ADD CONSTRAINT fk_mail_to_actor FOREIGN KEY (to_actor_id) REFERENCES public.actors(id);


--
-- Name: mcp_sessions fk_mcp_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_sessions
    ADD CONSTRAINT fk_mcp_actor FOREIGN KEY (actor_id) REFERENCES public.actors(id);


--
-- Name: virtual_agent_usage fk_vau_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.virtual_agent_usage
    ADD CONSTRAINT fk_vau_actor FOREIGN KEY (actor_id) REFERENCES public.actors(id);


--
-- Name: namespace_permissions namespace_permissions_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespace_permissions
    ADD CONSTRAINT namespace_permissions_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.actors(id) ON DELETE CASCADE;


--
-- Name: user_sessions user_sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_sessions
    ADD CONSTRAINT user_sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id);


--
-- PostgreSQL database dump complete
--


