## [1.8.0](https://github.com/Leicas/matrisms/compare/v1.7.0...v1.8.0) (2026-07-30)


### Features

* retry failed VoIP.ms connections with configurable backoff ([2176861](https://github.com/Leicas/matrisms/commit/217686138722ae92ef4303daeceb2f45401c3694))


### Bug Fixes

* **coordinator:** recognize the poller component in bridge state ([83a6489](https://github.com/Leicas/matrisms/commit/83a64890f6909bbf05741ce2b266a050c8d764d9))

## [1.7.0](https://github.com/Leicas/matrisms/compare/v1.6.0...v1.7.0) (2026-07-27)


### Features

* sync Element room renames into the VoIP.ms phonebook ([0bd9d9c](https://github.com/Leicas/matrisms/commit/0bd9d9c70fe42b7cad4faa3ba570a1e72c84dfa5))

## [1.6.0](https://github.com/Leicas/matrisms/compare/v1.5.0...v1.6.0) (2026-07-27)


### Features

* detect glued-URL scrambles without paired punctuation ([70094f8](https://github.com/Leicas/matrisms/commit/70094f82913193733cdfd9f0840705a8c5d0356f))


### Bug Fixes

* decode Latin-1 bodies and create phonebook entries via setPhonebook ([c3c6d6b](https://github.com/Leicas/matrisms/commit/c3c6d6bf39a8cf3d945a6fde42870e9d9bae248c))

## [1.5.0](https://github.com/Leicas/matrisms/compare/v1.4.0...v1.5.0) (2026-07-27)


### Features

* repair multipart SMS scrambled by VoIP.ms arrival-order reassembly ([f0c09d8](https://github.com/Leicas/matrisms/commit/f0c09d8f09274152f5fae7a8fa12b80f222b0f62))


### Bug Fixes

* detect French 'attribué la mention' reactions and mangled quotes ([2a94058](https://github.com/Leicas/matrisms/commit/2a940581ffde76fc5b461a242d3bf1d6cd193d44))

## [1.4.0](https://github.com/Leicas/matrisms/compare/v1.3.0...v1.4.0) (2026-07-21)


### Features

* convert reaction-fallback texts into real Matrix reactions ([48d2489](https://github.com/Leicas/matrisms/commit/48d24892fa47f27e764418d784ba68b36224b3d1))

## [1.3.0](https://github.com/Leicas/matrisms/compare/v1.2.1...v1.3.0) (2026-07-21)


### Features

* VoIP.ms logo as bot avatar, space avatar, and network icon ([01b74f4](https://github.com/Leicas/matrisms/commit/01b74f4010c41e63b00bbf023c3ba14220717985))

## [1.2.1](https://github.com/Leicas/matrisms/compare/v1.2.0...v1.2.1) (2026-07-21)


### Bug Fixes

* keep same-second message segments in send order ([52f6973](https://github.com/Leicas/matrisms/commit/52f697395b4ecdbd07fbc7101168f0f809799ce5))

## [1.2.0](https://github.com/Leicas/matrisms/compare/v1.1.0...v1.2.0) (2026-07-21)


### Features

* shrink oversized outbound MMS images to fit the VoIP.ms cap ([886350d](https://github.com/Leicas/matrisms/commit/886350d1babd0416c9245b57a64d6efdc2c6383c))

## [1.1.0](https://github.com/Leicas/matrisms/compare/v1.0.2...v1.1.0) (2026-07-21)


### Features

* per-DID spaces, phonebook contact names, clearer sent/received ([69b3784](https://github.com/Leicas/matrisms/commit/69b378450ce25edabff412b5e9d3dcf9588d9f48))

## [1.0.2](https://github.com/Leicas/matrisms/compare/v1.0.1...v1.0.2) (2026-07-21)


### Bug Fixes

* register UserLogin metadata type and connect after interactive login ([c43e973](https://github.com/Leicas/matrisms/commit/c43e9735195e6eedf80920ddba273da9e13fc536))

## [1.0.1](https://github.com/Leicas/matrisms/compare/v1.0.0...v1.0.1) (2026-07-21)


### Bug Fixes

* POST as multipart/form-data — rest.php parses urlencoded bodies as SOAP ([207caac](https://github.com/Leicas/matrisms/commit/207caac94074566be4bbc60065c0c39cbe0cc4f9))

## 1.0.0 (2026-07-21)


### Features

* initial VoIP.ms SMS/MMS bridge on mautrix bridgev2 ([40f2dac](https://github.com/Leicas/matrisms/commit/40f2dac5b4c9c9747e8dcfb8dda17a38e4b54b75))
