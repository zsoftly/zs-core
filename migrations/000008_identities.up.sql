CREATE TABLE identities (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL REFERENCES brands (id),
    issuer text NOT NULL,
    subject text NOT NULL,
    keycloak_user_id text,
    email text,
    email_verified boolean NOT NULL,
    display_name text,
    last_seen_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT identities_issuer_length CHECK (char_length(issuer) BETWEEN 1 AND 1000),
    CONSTRAINT identities_subject_length CHECK (char_length(subject) BETWEEN 1 AND 255),
    CONSTRAINT identities_email_length CHECK (email IS NULL OR char_length(email) <= 320),
    CONSTRAINT identities_display_name_length CHECK (display_name IS NULL OR char_length(display_name) <= 200)
);

CREATE UNIQUE INDEX identities_brand_issuer_subject_key ON identities (brand_id, issuer, subject);
