# Changelog

## [0.2.0](https://github.com/majimtor/corinto/compare/v0.1.0...v0.2.0) (2026-11-27)


### Features

* **auth:** add the sessions of the panel ([#73](https://github.com/majimtor/corinto/issues/73)) ([1a0c90f](https://github.com/majimtor/corinto/commit/1a0c90f42190e2f8724cdb6d6057aff8f49ed5e5))
* **panel:** add the choice of period and the breakdowns by device and browser ([#81](https://github.com/majimtor/corinto/issues/81)) ([43f4f1b](https://github.com/majimtor/corinto/commit/43f4f1b0deccb1c6255d4cc1dc5c890ef11eb23c))
* **panel:** add the demo entry and the list of links ([#76](https://github.com/majimtor/corinto/issues/76)) ([e511ea7](https://github.com/majimtor/corinto/commit/e511ea75790de79c45bd4f32b1637680b4cca27f))
* **panel:** add the forms that create, change and delete a link ([#79](https://github.com/majimtor/corinto/issues/79)) ([2cbf049](https://github.com/majimtor/corinto/commit/2cbf04906b3a911ed00d29e184dd9baf2e405e39))
* **panel:** add the keys of the API ([#82](https://github.com/majimtor/corinto/issues/82)) ([d893585](https://github.com/majimtor/corinto/commit/d893585445349d16413ce8c22b833f1ecf6c5a59))
* **panel:** add the page of a link ([#70](https://github.com/majimtor/corinto/issues/70)) ([fa5478a](https://github.com/majimtor/corinto/commit/fa5478a3715e0f747ae9413a38cc6d19cb820afe))
* **panel:** add the status page ([#83](https://github.com/majimtor/corinto/issues/83)) ([c1bd525](https://github.com/majimtor/corinto/commit/c1bd525fd2a35396e6ba0915e67f0c2308ed7683))
* **seed:** write ninety days of synthetic history ([#78](https://github.com/majimtor/corinto/issues/78)) ([2a5f1bb](https://github.com/majimtor/corinto/commit/2a5f1bb5aa11391094178fe7f83802969c8515bf))


### Bug Fixes

* **deploy:** ask the database over TCP whether it is ready ([#88](https://github.com/majimtor/corinto/issues/88)) ([7bc9ba5](https://github.com/majimtor/corinto/commit/7bc9ba527f975f5f0bfeeb161a3cbcec3f1a491e))

## 0.1.0 (2026-11-20)


### Features

* **api:** add key authentication and its limits ([#24](https://github.com/majimtor/corinto/issues/24)) ([6720d1b](https://github.com/majimtor/corinto/commit/6720d1bccb61d313ca8497c01183f3e3d456399c))
* **api:** add the metrics of a link ([#43](https://github.com/majimtor/corinto/issues/43)) ([afcc789](https://github.com/majimtor/corinto/commit/afcc789cb42ccab46f4d84849952b94283a44f93))
* **api:** add the status of the ingestion ([#45](https://github.com/majimtor/corinto/issues/45)) ([26af61b](https://github.com/majimtor/corinto/commit/26af61bbd6471fb113c79580596aef505ed451dc))
* **api:** change, disable, enable and delete links ([#26](https://github.com/majimtor/corinto/issues/26)) ([98e1322](https://github.com/majimtor/corinto/commit/98e13223eaf985b0352f04fd447a87fe809c0ec5))
* **api:** create, read and list links ([#25](https://github.com/majimtor/corinto/issues/25)) ([a846168](https://github.com/majimtor/corinto/commit/a8461683686c2886e27c7a50fe02965995a88320))
* **api:** serve the contract with its first operations ([#22](https://github.com/majimtor/corinto/issues/22)) ([74b734b](https://github.com/majimtor/corinto/commit/74b734b467840ad97826e6419c157ab32c439ee5))
* **auth:** issue, resolve and revoke API keys ([#17](https://github.com/majimtor/corinto/issues/17)) ([3fad8a6](https://github.com/majimtor/corinto/commit/3fad8a684d0b5a25ca0e721b37bda92781274af1))
* **bench:** add the load scenario ([#52](https://github.com/majimtor/corinto/issues/52)) ([bb16576](https://github.com/majimtor/corinto/commit/bb165765a40225ee940b353f2799568f54b00444))
* **classify:** classify traffic by versioned rules ([#32](https://github.com/majimtor/corinto/issues/32)) ([93afe1a](https://github.com/majimtor/corinto/commit/93afe1ad062b0929b7a5030c0b526f601388918c))
* **contract:** add the contract skeleton ([#6](https://github.com/majimtor/corinto/issues/6)) ([fca3b8d](https://github.com/majimtor/corinto/commit/fca3b8de9c5b29a3d51e08c5b5009a136286866d))
* **ingest:** add the buffer and the inbox ([#33](https://github.com/majimtor/corinto/issues/33)) ([4433c10](https://github.com/majimtor/corinto/commit/4433c1062b1287c98963d498f1936c4530c9f0ef))
* **link:** add short identifiers and aliases ([#12](https://github.com/majimtor/corinto/issues/12)) ([74ec057](https://github.com/majimtor/corinto/commit/74ec057235bdde65e967ca168ccf46b5918a7664))
* **link:** change and delete links ([#16](https://github.com/majimtor/corinto/issues/16)) ([38ccf5c](https://github.com/majimtor/corinto/commit/38ccf5c4fa23771a490a89b09d70389dc26b298c))
* **link:** store, read and list links ([#15](https://github.com/majimtor/corinto/issues/15)) ([c6b4f35](https://github.com/majimtor/corinto/commit/c6b4f35f6ad05d24e068081f2b588bb66ad9b576))
* **link:** validate and normalise destinations ([#13](https://github.com/majimtor/corinto/issues/13)) ([ac9b026](https://github.com/majimtor/corinto/commit/ac9b026f4f18d20ceda4780d63230beeb5173800))
* **operator:** block links and workspaces ([#19](https://github.com/majimtor/corinto/issues/19)) ([e6d5733](https://github.com/majimtor/corinto/commit/e6d5733b12f62f52fcb76f023c3b3173dedeef54))
* **operator:** create workspaces and issue and revoke keys ([#18](https://github.com/majimtor/corinto/issues/18)) ([7be0c10](https://github.com/majimtor/corinto/commit/7be0c10717640b71f04cf975bc8b8867e3f41333))
* **operator:** send dead letters back to the inbox ([#38](https://github.com/majimtor/corinto/issues/38)) ([ff9cb33](https://github.com/majimtor/corinto/commit/ff9cb33d877b41dfebdcb616ea7404bdbb18e4ce))
* **postgres:** add the migration runner and the first schema ([#14](https://github.com/majimtor/corinto/issues/14)) ([0b1d9c1](https://github.com/majimtor/corinto/commit/0b1d9c1d322fe5ef5ce3b4742ef87e4f8b3eaa5a))
* **redirect:** limit the rate per client address ([#31](https://github.com/majimtor/corinto/issues/31)) ([b5d895d](https://github.com/majimtor/corinto/commit/b5d895d52dba629b03e9e5cabfb367090f7f427b))
* **redirect:** measure the service time ([#51](https://github.com/majimtor/corinto/issues/51)) ([7fbc2e7](https://github.com/majimtor/corinto/commit/7fbc2e753c7003817496fe3c864bc63bd6c26a7e))
* **redirect:** record the access events ([#34](https://github.com/majimtor/corinto/issues/34)) ([58602e9](https://github.com/majimtor/corinto/commit/58602e944b80aa9ea1fbde7cddf30bd1634bde05))
* **redirect:** run the redirect process ([#30](https://github.com/majimtor/corinto/issues/30)) ([9337c63](https://github.com/majimtor/corinto/commit/9337c6342f46079a2f5faf4648101acd38c470b9))
* **redirect:** say how old the copy of the links is ([#44](https://github.com/majimtor/corinto/issues/44)) ([112d2e8](https://github.com/majimtor/corinto/commit/112d2e83f4711a770041b18abdd55bbc36661002))
* **redirect:** serve the redirect contract ([#28](https://github.com/majimtor/corinto/issues/28)) ([b4e6dd0](https://github.com/majimtor/corinto/commit/b4e6dd0519424e5da07431a908ae9e2de52bb563))
* **resolve:** add the in-memory replica ([#27](https://github.com/majimtor/corinto/issues/27)) ([d7661ed](https://github.com/majimtor/corinto/commit/d7661ed7dc8d960217515799bead197491a15921))
* **rollup:** add retention ([#40](https://github.com/majimtor/corinto/issues/40)) ([98872d7](https://github.com/majimtor/corinto/commit/98872d73521a3887ff4fe852aee79ffca2ec8744))
* **rollup:** consume the inbox into aggregates ([#35](https://github.com/majimtor/corinto/issues/35)) ([a065c7d](https://github.com/majimtor/corinto/commit/a065c7da5cc57b32863f9b0371a9ca18761c99d6))
* **rollup:** read the metrics of a link ([#42](https://github.com/majimtor/corinto/issues/42)) ([01395e6](https://github.com/majimtor/corinto/commit/01395e68344212dc8bd68059e1d915bd190eb5cc))
* **rollup:** record the versions of the rules as events are counted ([#41](https://github.com/majimtor/corinto/issues/41)) ([2c31e61](https://github.com/majimtor/corinto/commit/2c31e61e769a67b20094158d53f8bd75b1333312))
* **rollup:** retry failed events and dead-letter them ([#37](https://github.com/majimtor/corinto/issues/37)) ([ad5cca9](https://github.com/majimtor/corinto/commit/ad5cca91d2e708ccc76ea1f06cbb5053888ed746))
* **seed:** add the seed with links and keys ([#47](https://github.com/majimtor/corinto/issues/47)) ([98849df](https://github.com/majimtor/corinto/commit/98849dfc03165c4c26a0e8b1e32a02b28edac83c))
* **seed:** seed the system when it starts ([#48](https://github.com/majimtor/corinto/issues/48)) ([d30d8be](https://github.com/majimtor/corinto/commit/d30d8bee1b9ef76883e2cee882c484b8e61d199c))
* **worker:** run the worker process ([#36](https://github.com/majimtor/corinto/issues/36)) ([7d6aa53](https://github.com/majimtor/corinto/commit/7d6aa534bd936445c42db5dd0195f935123787bd))


### Bug Fixes

* **seed:** keep the links of a demo workspace to the hosts of the seed ([#58](https://github.com/majimtor/corinto/issues/58)) ([afeaa27](https://github.com/majimtor/corinto/commit/afeaa27374f54e7fbef9dfdd54235020e2a3100b))
