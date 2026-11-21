-- Baseline: the schema GORM AutoMigrate built before goose, from pg_dump --schema-only --no-owner
-- --no-privileges of a fresh database, without SET lines, comments or the public. qualifier, so it
-- creates in the current schema. Never edit it: a schema change is a new migration (docs/data-model.md).

CREATE TABLE billing_checkout_attempts (
    id bigint NOT NULL,
    contract_id bigint NOT NULL,
    reference text NOT NULL,
    plan_id text NOT NULL,
    first_amount_cents bigint NOT NULL,
    stripe_price_id text NOT NULL,
    customer_email text DEFAULT ''::text NOT NULL,
    creation_started_at timestamp with time zone NOT NULL,
    stripe_session_id text,
    checkout_url text,
    decline_code text,
    state text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT attempt_first_amount_cents CHECK ((first_amount_cents > 0)),
    CONSTRAINT attempt_state CHECK ((state = ANY (ARRAY['creating'::text, 'open'::text, 'processing'::text, 'expiring'::text, 'uncertain'::text, 'expired'::text, 'failed'::text, 'paid'::text])))
);

CREATE SEQUENCE billing_checkout_attempts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE billing_checkout_attempts_id_seq OWNED BY billing_checkout_attempts.id;

CREATE TABLE billing_contracts (
    id bigint NOT NULL,
    enrollment_id bigint NOT NULL,
    reference text NOT NULL,
    plan_id text NOT NULL,
    plan_name text NOT NULL,
    installment_count bigint NOT NULL,
    total_cents bigint NOT NULL,
    funded_at timestamp with time zone,
    stripe_customer_id text,
    synced_customer_name text,
    synced_customer_email text,
    review_reason text,
    withdrawn_at timestamp with time zone,
    withdrawn_by text,
    last_synced_at timestamp with time zone,
    next_sync_at timestamp with time zone,
    lease_token text,
    lease_expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT contract_installment_count CHECK ((installment_count > 0)),
    CONSTRAINT contract_total_cents CHECK ((total_cents > 0))
);

CREATE SEQUENCE billing_contracts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE billing_contracts_id_seq OWNED BY billing_contracts.id;

CREATE TABLE billing_obligations (
    id bigint NOT NULL,
    contract_id bigint NOT NULL,
    reference text NOT NULL,
    kind text NOT NULL,
    number bigint NOT NULL,
    amount_cents bigint NOT NULL,
    stripe_price_id text NOT NULL,
    due_at timestamp with time zone,
    suspension_id bigint,
    deadline_episode_id bigint,
    collection_disposition text DEFAULT 'collect'::text NOT NULL,
    stripe_invoice_id text,
    stripe_invoice_item_id text,
    invoice_creation_started_at timestamp with time zone,
    item_creation_started_at timestamp with time zone,
    scheduled_finalization_at timestamp with time zone,
    latest_stripe_status text,
    amount_paid_cents bigint DEFAULT 0 NOT NULL,
    amount_remaining_cents bigint DEFAULT 0 NOT NULL,
    other_settlement_cents bigint DEFAULT 0 NOT NULL,
    first_settled_at timestamp with time zone,
    failure_or_action_required boolean DEFAULT false NOT NULL,
    retry_count bigint DEFAULT 0 NOT NULL,
    next_retry_at timestamp with time zone,
    review_reason text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT obligation_amount_cents CHECK ((amount_cents > 0)),
    CONSTRAINT obligation_amount_paid CHECK ((amount_paid_cents >= 0)),
    CONSTRAINT obligation_amount_remaining CHECK ((amount_remaining_cents >= 0)),
    CONSTRAINT obligation_disposition CHECK ((collection_disposition = ANY (ARRAY['collect'::text, 'cancel'::text]))),
    CONSTRAINT obligation_kind CHECK ((kind = ANY (ARRAY['tuition'::text, 'reinstatement'::text]))),
    CONSTRAINT obligation_other_settlement CHECK ((other_settlement_cents >= 0))
);

CREATE SEQUENCE billing_obligations_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE billing_obligations_id_seq OWNED BY billing_obligations.id;

CREATE TABLE billing_payments (
    id text NOT NULL,
    contract_id bigint NOT NULL,
    gross_received_cents bigint NOT NULL,
    gross_refunded_cents bigint DEFAULT 0 NOT NULL,
    payment_time timestamp with time zone NOT NULL,
    dispute_or_fraud_review_state text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT payment_gross_received CHECK ((gross_received_cents >= 0)),
    CONSTRAINT payment_gross_refunded CHECK ((gross_refunded_cents >= 0))
);

CREATE TABLE billing_suspensions (
    id bigint NOT NULL,
    contract_id bigint NOT NULL,
    triggering_tuition_installment_number bigint CONSTRAINT billing_suspensions_triggering_tuition_installment_num_not_null NOT NULL,
    started_at timestamp with time zone NOT NULL,
    reinstatement_requested_at timestamp with time zone,
    requested_by text,
    closed_at timestamp with time zone,
    close_reason text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT suspension_close_reason CHECK (((close_reason IS NULL) OR (close_reason = ANY (ARRAY['reinstated'::text, 'corrected'::text]))))
);

CREATE SEQUENCE billing_suspensions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE billing_suspensions_id_seq OWNED BY billing_suspensions.id;

CREATE TABLE billing_webhook_events (
    id text NOT NULL,
    event_type text NOT NULL,
    resource_id text NOT NULL,
    processed_at timestamp with time zone,
    retry_at timestamp with time zone,
    retry_count bigint DEFAULT 0 NOT NULL,
    lease_token text,
    lease_expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

CREATE TABLE enrollments (
    id bigint NOT NULL,
    link_id bigint NOT NULL,
    session_hash bytea NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    phone text DEFAULT ''::text NOT NULL,
    municipality text DEFAULT ''::text NOT NULL,
    date_of_birth text DEFAULT ''::text NOT NULL,
    gender text DEFAULT ''::text NOT NULL,
    pin text DEFAULT ''::text NOT NULL,
    pin_checks bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    submitted_at timestamp with time zone,
    terms_accepted_at timestamp with time zone,
    program_start_date text DEFAULT ''::text NOT NULL,
    paid_at timestamp with time zone,
    expires_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    canceled_at timestamp with time zone,
    canceled_by text DEFAULT ''::text NOT NULL,
    CONSTRAINT session_hash_size CHECK ((octet_length(session_hash) = 32))
);

CREATE SEQUENCE enrollments_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE enrollments_id_seq OWNED BY enrollments.id;

CREATE TABLE links (
    id bigint NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    token text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    expires_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    canceled_at timestamp with time zone,
    created_by text DEFAULT ''::text NOT NULL,
    canceled_by text DEFAULT ''::text NOT NULL,
    enrollment_count bigint DEFAULT 0 NOT NULL,
    CONSTRAINT link_token_length CHECK ((char_length(token) = 43))
);

CREATE SEQUENCE links_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE links_id_seq OWNED BY links.id;

CREATE TABLE sessions (
    token text NOT NULL,
    data bytea NOT NULL,
    expiry timestamp with time zone NOT NULL
);

CREATE TABLE settings (
    id bigint NOT NULL,
    link_lifetime_days bigint NOT NULL,
    program_start_date text DEFAULT ''::text NOT NULL,
    bootcamp_date text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    updated_by text NOT NULL,
    CONSTRAINT link_lifetime_days_range CHECK (((link_lifetime_days >= 2) AND (link_lifetime_days <= 14))),
    CONSTRAINT settings_single_row CHECK ((id = 1))
);

CREATE TABLE students (
    id bigint NOT NULL,
    enrollment_id bigint,
    name text DEFAULT ''::text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    phone text DEFAULT ''::text NOT NULL,
    municipality text DEFAULT ''::text NOT NULL,
    date_of_birth text DEFAULT ''::text NOT NULL,
    gender text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    withdrawn_at timestamp with time zone,
    pin text DEFAULT ''::text NOT NULL,
    "group" text DEFAULT 'Básico'::text NOT NULL,
    cohort text NOT NULL,
    CONSTRAINT student_cohort_valid CHECK (((cohort = ''::text) OR (cohort ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'::text))),
    CONSTRAINT student_group_valid CHECK (("group" = ANY (ARRAY['Básico'::text, 'Regular'::text])))
);

CREATE SEQUENCE students_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE students_id_seq OWNED BY students.id;

CREATE TABLE users (
    username text NOT NULL,
    password_hash text NOT NULL,
    auth_version text NOT NULL
);

ALTER TABLE ONLY billing_checkout_attempts ALTER COLUMN id SET DEFAULT nextval('billing_checkout_attempts_id_seq'::regclass);

ALTER TABLE ONLY billing_contracts ALTER COLUMN id SET DEFAULT nextval('billing_contracts_id_seq'::regclass);

ALTER TABLE ONLY billing_obligations ALTER COLUMN id SET DEFAULT nextval('billing_obligations_id_seq'::regclass);

ALTER TABLE ONLY billing_suspensions ALTER COLUMN id SET DEFAULT nextval('billing_suspensions_id_seq'::regclass);

ALTER TABLE ONLY enrollments ALTER COLUMN id SET DEFAULT nextval('enrollments_id_seq'::regclass);

ALTER TABLE ONLY links ALTER COLUMN id SET DEFAULT nextval('links_id_seq'::regclass);

ALTER TABLE ONLY students ALTER COLUMN id SET DEFAULT nextval('students_id_seq'::regclass);

ALTER TABLE ONLY billing_checkout_attempts
    ADD CONSTRAINT billing_checkout_attempts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY billing_contracts
    ADD CONSTRAINT billing_contracts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY billing_obligations
    ADD CONSTRAINT billing_obligations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY billing_payments
    ADD CONSTRAINT billing_payments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY billing_suspensions
    ADD CONSTRAINT billing_suspensions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY billing_webhook_events
    ADD CONSTRAINT billing_webhook_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY enrollments
    ADD CONSTRAINT enrollments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY links
    ADD CONSTRAINT links_pkey PRIMARY KEY (id);

ALTER TABLE ONLY sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (token);

ALTER TABLE ONLY settings
    ADD CONSTRAINT settings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY students
    ADD CONSTRAINT students_pkey PRIMARY KEY (id);

ALTER TABLE ONLY users
    ADD CONSTRAINT users_pkey PRIMARY KEY (username);

CREATE UNIQUE INDEX idx_active_checkout_attempt ON billing_checkout_attempts USING btree (contract_id) WHERE ((state = 'creating'::text) OR (state = 'open'::text) OR (state = 'processing'::text) OR (state = 'expiring'::text) OR (state = 'uncertain'::text));

CREATE UNIQUE INDEX idx_active_suspension ON billing_suspensions USING btree (contract_id) WHERE (closed_at IS NULL);

CREATE INDEX idx_billing_checkout_attempts_contract_id ON billing_checkout_attempts USING btree (contract_id);

CREATE UNIQUE INDEX idx_billing_checkout_attempts_reference ON billing_checkout_attempts USING btree (reference);

CREATE UNIQUE INDEX idx_billing_checkout_attempts_stripe_session_id ON billing_checkout_attempts USING btree (stripe_session_id);

CREATE UNIQUE INDEX idx_billing_contracts_enrollment_id ON billing_contracts USING btree (enrollment_id);

CREATE UNIQUE INDEX idx_billing_contracts_reference ON billing_contracts USING btree (reference);

CREATE UNIQUE INDEX idx_billing_contracts_stripe_customer_id ON billing_contracts USING btree (stripe_customer_id);

CREATE INDEX idx_billing_obligations_contract_id ON billing_obligations USING btree (contract_id);

CREATE INDEX idx_billing_obligations_deadline_episode_id ON billing_obligations USING btree (deadline_episode_id);

CREATE UNIQUE INDEX idx_billing_obligations_reference ON billing_obligations USING btree (reference);

CREATE UNIQUE INDEX idx_billing_obligations_stripe_invoice_id ON billing_obligations USING btree (stripe_invoice_id);

CREATE UNIQUE INDEX idx_billing_obligations_stripe_invoice_item_id ON billing_obligations USING btree (stripe_invoice_item_id);

CREATE UNIQUE INDEX idx_billing_obligations_suspension_id ON billing_obligations USING btree (suspension_id);

CREATE INDEX idx_billing_payments_contract_id ON billing_payments USING btree (contract_id);

CREATE INDEX idx_billing_suspensions_contract_id ON billing_suspensions USING btree (contract_id);

CREATE UNIQUE INDEX idx_enrollment_pin ON enrollments USING btree (pin) WHERE (pin <> ''::text);

CREATE INDEX idx_enrollments_link_id ON enrollments USING btree (link_id);

CREATE UNIQUE INDEX idx_enrollments_session_hash ON enrollments USING btree (session_hash);

CREATE UNIQUE INDEX idx_links_token ON links USING btree (token);

CREATE INDEX idx_pending_events ON billing_webhook_events USING btree (created_at) WHERE (processed_at IS NULL);

CREATE UNIQUE INDEX idx_student_pin ON students USING btree (pin) WHERE (pin <> ''::text);

CREATE UNIQUE INDEX idx_students_enrollment_id ON students USING btree (enrollment_id);

CREATE UNIQUE INDEX idx_tuition_contract_number ON billing_obligations USING btree (contract_id, number) WHERE (kind = 'tuition'::text);

CREATE INDEX sessions_expiry_idx ON sessions USING btree (expiry);

ALTER TABLE ONLY billing_checkout_attempts
    ADD CONSTRAINT fk_billing_checkout_attempts_contract FOREIGN KEY (contract_id) REFERENCES billing_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT;

ALTER TABLE ONLY billing_contracts
    ADD CONSTRAINT fk_billing_contracts_enrollment FOREIGN KEY (enrollment_id) REFERENCES enrollments(id) ON UPDATE RESTRICT ON DELETE RESTRICT;

ALTER TABLE ONLY billing_obligations
    ADD CONSTRAINT fk_billing_obligations_contract FOREIGN KEY (contract_id) REFERENCES billing_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT;

ALTER TABLE ONLY billing_payments
    ADD CONSTRAINT fk_billing_payments_contract FOREIGN KEY (contract_id) REFERENCES billing_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT;

ALTER TABLE ONLY billing_suspensions
    ADD CONSTRAINT fk_billing_suspensions_contract FOREIGN KEY (contract_id) REFERENCES billing_contracts(id) ON UPDATE RESTRICT ON DELETE RESTRICT;

ALTER TABLE ONLY students
    ADD CONSTRAINT fk_enrollments_student FOREIGN KEY (enrollment_id) REFERENCES enrollments(id) ON UPDATE RESTRICT ON DELETE RESTRICT;

ALTER TABLE ONLY enrollments
    ADD CONSTRAINT fk_links_enrollments FOREIGN KEY (link_id) REFERENCES links(id) ON UPDATE RESTRICT ON DELETE RESTRICT;
