# 0008. Access event model and data minimisation

- Status: accepted
- Date: 2026-11-14
- Deciders: the maintainer

## Context

Every request to a short link comes from an unknown visitor and carries an
IP address, a user-agent and a referrer. Under the GDPR an IP address is an
online identifier that can identify a person (recital 30). The Spanish data
protection authority treats a hash as pseudonymisation, not anonymisation,
unless a re-identification analysis shows otherwise (AEPD and EDPS, 2019);
the IPv4 space has 2^32 values and can be enumerated. The authority's guide
on audience measurement (January 2024) lists what it considers strictly
necessary and exempt from consent: audience per page, and referrer, device,
browser and geographic area aggregated daily. The European Data Protection
Board places links that carry tracking information within Article 5(3) of
the ePrivacy Directive and does not analyse the exemptions (Guidelines
2/2023, version 2.0).

## Decision

An access event describes a request in coarse, closed categories and
carries nothing that identifies a visitor or links two accesses to each
other, or an access to a conversion. PLAN.md section 5 lists what it
carries and what it never carries.

Two things are used in passing and never stored, both in the memory of the
process that handles the request: the client address, as the key of a rate
limiter after a keyed hash that expires, and the user-agent string, which
is classified inside the request and kept only as the key of a bounded
cache of classifications. PLAN.md section 17 holds the inventory.

Neither the country nor the operating system of the access is recorded.
Metrics are designed inside the authority's list (ADR 0010).

## Alternatives considered

- Store a hashed or truncated IP address. It would allow unique counts and
  abuse analysis. It is pseudonymised personal data, with the obligations
  that follow.
- Store the raw user-agent to reclassify history when rules improve. It is
  a high-entropy string kept for a convenience.
- A per-click identifier, to attribute each conversion to a click. It is
  per-person tracking; conversions are attributed to the link instead.
- The operating system family as a third enumeration. A breakdown managers
  ask for. The authority's list names device and browser, not operating
  system, and the claim of staying inside it would need an exception.

## Consequences

- Positive: the stored data identifies nobody; the model is easy to
  explain; whoever deploys the software starts from a small inventory.
- Negative: history cannot be reclassified, and unique counts are not
  possible without a transient identifier, which is why approximate unique
  clicks have their own later decision.
- Strongest argument against: by a strict reading, timestamps to the second
  with device and referrer could still single out an access in a small
  link. The risk is accepted and described in `docs/privacy.md`.

## Reopen when

The slice for approximate unique clicks starts, or the guidance cited above
changes. This is a technical reading of the sources, not legal advice.

## Links

ADR 0009, ADR 0010. PLAN.md sections 5 and 17. Regulation (EU) 2016/679;
Spanish Act 34/2002, article 22.2; AEPD and EDPS study on hashing as a
pseudonymisation technique (October 2019); AEPD guide on cookies for
audience measurement tools (January 2024); EDPB Guidelines 2/2023 v2.0 (7
October 2024); consulted 2026-11-12.
