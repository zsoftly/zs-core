-- Every row that names an identity also carries its brand, and its foreign
-- key covers both, so a tenant, a membership or a creation record can never
-- name an identity of another brand. identities (brand_id, id) is unique
-- because id is the primary key; the index lets the foreign keys reference
-- the pair.
CREATE UNIQUE INDEX identities_brand_id_key ON identities (brand_id, id);

ALTER TABLE tenants
    ADD CONSTRAINT tenants_created_by_identity_fkey
    FOREIGN KEY (brand_id, created_by_identity_id) REFERENCES identities (brand_id, id);

ALTER TABLE personal_tenant_principals
    ADD CONSTRAINT personal_tenant_principals_identity_fkey
    FOREIGN KEY (brand_id, identity_id) REFERENCES identities (brand_id, id);

ALTER TABLE memberships
    ADD CONSTRAINT memberships_identity_fkey
    FOREIGN KEY (brand_id, identity_id) REFERENCES identities (brand_id, id);

ALTER TABLE tenant_provisioning
    ADD CONSTRAINT tenant_provisioning_created_by_identity_fkey
    FOREIGN KEY (brand_id, created_by_identity_id) REFERENCES identities (brand_id, id);
