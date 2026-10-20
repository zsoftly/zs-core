// Package schemas embeds the JSON Schemas under api/schemas so packages can
// validate against the same files the documentation points at.
package schemas

import _ "embed"

// BrandManifest is api/schemas/brand-manifest.json.
//
//go:embed brand-manifest.json
var BrandManifest []byte

// BrandManifestURL is the $id of BrandManifest.
const BrandManifestURL = "urn:zsoftly:zs-core:schema:brand-manifest:1"
