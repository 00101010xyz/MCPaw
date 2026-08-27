-- Adds the per-instance HTTP Host header override.
--
-- Some upstream services (Zotero's local API among them) validate the Host
-- header as a DNS-rebinding defense and reject any value that isn't a
-- loopback name — which is exactly the problem a container hits reaching such
-- a service via host.docker.internal instead of 127.0.0.1. This column lets
-- an instance present the Host header the upstream expects while still
-- connecting to the address that's actually reachable.

ALTER TABLE instances ADD COLUMN host_header_override TEXT NOT NULL DEFAULT '';
