# Changelog

All notable changes to genai-otel-bridge. Generated from Conventional Commits.
## [3.1.0](https://github.com/rknightion/genai-otel-bridge/compare/v3.0.1...v3.1.0) (2026-07-23)


### Features

* AWS ECS deployment target (DynamoDB-backed HA) ([#13](https://github.com/rknightion/genai-otel-bridge/issues/13)) ([f6f0d61](https://github.com/rknightion/genai-otel-bridge/commit/f6f0d61df6e3179ae6f7ea88e77fa9cb4538c4fc))
* **docs:** align docs site with m7kni.io brand + server-side SEO/LLM metadata ([c272581](https://github.com/rknightion/genai-otel-bridge/commit/c2725814fedf2e4f1a117392eac3c94ad63a3729)), closes [#26](https://github.com/rknightion/genai-otel-bridge/issues/26)
* generate drift-guarded telemetry catalogue into docs/telemetry.md ([cfd4ad4](https://github.com/rknightion/genai-otel-bridge/commit/cfd4ad46ab3f3843a8e94673295bb09c8ea553b5))
* **langsmith:** add usage loop for platform cost-driver metrics ([f75be9a](https://github.com/rknightion/genai-otel-bridge/commit/f75be9a6660fce190ed71ffc2ae601b76f04be59))
* third-party license notices + SBOMs as release artifacts ([f8cf300](https://github.com/rknightion/genai-otel-bridge/commit/f8cf300a3d47e7a1fe862fbb2cf3c7170b37fcb3))


### Bug Fixes

* **app:** fail fast on unknown loop names and bound the health server's header read ([609112c](https://github.com/rknightion/genai-otel-bridge/commit/609112c9027b2a85634ea1e6b9ce67df024efa05)), closes [#40](https://github.com/rknightion/genai-otel-bridge/issues/40) [#72](https://github.com/rknightion/genai-otel-bridge/issues/72)
* block unspecified address in SSRF guard and correct publish-pipeline docs ([44c8c9e](https://github.com/rknightion/genai-otel-bridge/commit/44c8c9e85912a727938063f54b4179e9e5891b31)), closes [#96](https://github.com/rknightion/genai-otel-bridge/issues/96) [#67](https://github.com/rknightion/genai-otel-bridge/issues/67)
* **checkpoint:** reject unencodable watermark times and stop masking failed file writes ([44cd89a](https://github.com/rknightion/genai-otel-bridge/commit/44cd89a442a401036db8fcbfbe91ee4e83b4fb90)), closes [#81](https://github.com/rknightion/genai-otel-bridge/issues/81) [#82](https://github.com/rknightion/genai-otel-bridge/issues/82)
* **ci,docs:** gate edge publish on CI, align make ci/gate, and correct doc drift ([54fd99f](https://github.com/rknightion/genai-otel-bridge/commit/54fd99f5f99e3b7df97339c14ec3fb9146851edb)), closes [#107](https://github.com/rknightion/genai-otel-bridge/issues/107) [#109](https://github.com/rknightion/genai-otel-bridge/issues/109) [#115](https://github.com/rknightion/genai-otel-bridge/issues/115) [#118](https://github.com/rknightion/genai-otel-bridge/issues/118) [#123](https://github.com/rknightion/genai-otel-bridge/issues/123) [#131](https://github.com/rknightion/genai-otel-bridge/issues/131) [#135](https://github.com/rknightion/genai-otel-bridge/issues/135) [#136](https://github.com/rknightion/genai-otel-bridge/issues/136)
* **ci:** correct image-tag docs, remove orphaned VERSION, retire superseded pin note ([19d0612](https://github.com/rknightion/genai-otel-bridge/commit/19d061266288eb44c749b033937b9fcd89505c0a)), closes [#36](https://github.com/rknightion/genai-otel-bridge/issues/36) [#69](https://github.com/rknightion/genai-otel-bridge/issues/69) [#70](https://github.com/rknightion/genai-otel-bridge/issues/70)
* **cleanup:** retain the leader Lease alongside the checkpoint on uninstall ([90bc41a](https://github.com/rknightion/genai-otel-bridge/commit/90bc41a852dfc8b150650612a094c2c563c159b9)), closes [#33](https://github.com/rknightion/genai-otel-bridge/issues/33)
* **config:** close validation gaps that let broken configs load ([96332f2](https://github.com/rknightion/genai-otel-bridge/commit/96332f2f93f09468817c647b97010ba90b0ca7e9)), closes [#38](https://github.com/rknightion/genai-otel-bridge/issues/38) [#39](https://github.com/rknightion/genai-otel-bridge/issues/39) [#41](https://github.com/rknightion/genai-otel-bridge/issues/41) [#46](https://github.com/rknightion/genai-otel-bridge/issues/46) [#52](https://github.com/rknightion/genai-otel-bridge/issues/52)
* **config:** default queue depth/bytes and reject the dead telemetry metric_interval ([e1b99ae](https://github.com/rknightion/genai-otel-bridge/commit/e1b99ae3ecc65867e1f78bcd7c22b6b8171fc222)), closes [#113](https://github.com/rknightion/genai-otel-bridge/issues/113) [#114](https://github.com/rknightion/genai-otel-bridge/issues/114)
* **coordinate:** bound renew, fix self-lockout, classify leadership-loss, require identity ([a17b740](https://github.com/rknightion/genai-otel-bridge/commit/a17b7407dee8986ee20c27df8c30c4f67965ac51)), closes [#30](https://github.com/rknightion/genai-otel-bridge/issues/30) [#74](https://github.com/rknightion/genai-otel-bridge/issues/74) [#84](https://github.com/rknightion/genai-otel-bridge/issues/84) [#87](https://github.com/rknightion/genai-otel-bridge/issues/87)
* **deploy:** add graph-unavailable alert, HEALTHCHECK, REPLICAS env, netpol scoping, dockerignore ([61b626b](https://github.com/rknightion/genai-otel-bridge/commit/61b626b82013d04b85fcfb1de4a488f77b3626ff)), closes [#119](https://github.com/rknightion/genai-otel-bridge/issues/119) [#124](https://github.com/rknightion/genai-otel-bridge/issues/124) [#125](https://github.com/rknightion/genai-otel-bridge/issues/125) [#126](https://github.com/rknightion/genai-otel-bridge/issues/126) [#127](https://github.com/rknightion/genai-otel-bridge/issues/127) [#128](https://github.com/rknightion/genai-otel-bridge/issues/128)
* **deploy:** correct ECS IAM/GOMEMLIMIT, chart image pin, ESO/netpol rendering, and drift ([8b85de9](https://github.com/rknightion/genai-otel-bridge/commit/8b85de950b5e4cc2e188d40541c571725b5e28c7)), closes [#31](https://github.com/rknightion/genai-otel-bridge/issues/31) [#32](https://github.com/rknightion/genai-otel-bridge/issues/32) [#47](https://github.com/rknightion/genai-otel-bridge/issues/47) [#49](https://github.com/rknightion/genai-otel-bridge/issues/49) [#50](https://github.com/rknightion/genai-otel-bridge/issues/50) [#85](https://github.com/rknightion/genai-otel-bridge/issues/85) [#86](https://github.com/rknightion/genai-otel-bridge/issues/86)
* **deps:** update go modules (non-major) ([#151](https://github.com/rknightion/genai-otel-bridge/issues/151)) ([6f6ac37](https://github.com/rknightion/genai-otel-bridge/commit/6f6ac37cc40fdb537bd654fbab5a9d9b13a063e0))
* **deps:** update go modules (non-major) ([#156](https://github.com/rknightion/genai-otel-bridge/issues/156)) ([960e9d4](https://github.com/rknightion/genai-otel-bridge/commit/960e9d428f87cfb2ac61c71d86a9dd5ed32323a2))
* **deps:** update go modules (non-major) ([#159](https://github.com/rknightion/genai-otel-bridge/issues/159)) ([c9857ce](https://github.com/rknightion/genai-otel-bridge/commit/c9857ce0f90cc191b452e886fd111096c836b312))
* **deps:** update go modules (non-major) ([#174](https://github.com/rknightion/genai-otel-bridge/issues/174)) ([761a872](https://github.com/rknightion/genai-otel-bridge/commit/761a872f772f6bd4e75efea165f71cae3b39a87a))
* **deps:** update go modules (non-major) ([#22](https://github.com/rknightion/genai-otel-bridge/issues/22)) ([dd2f4b4](https://github.com/rknightion/genai-otel-bridge/commit/dd2f4b455cf0b315203c9eb47e42fbdc796ff62a))
* **deps:** update go modules (non-major) ([#27](https://github.com/rknightion/genai-otel-bridge/issues/27)) ([d6f1811](https://github.com/rknightion/genai-otel-bridge/commit/d6f18119e37a4f2ed84ebcac76bc3420a9a03d8f))
* **deps:** update kubernetes libraries ([#6](https://github.com/rknightion/genai-otel-bridge/issues/6)) ([71166a3](https://github.com/rknightion/genai-otel-bridge/commit/71166a34d4e8c9cfab655048056a0b19d8020670))
* **deps:** update kubernetes libraries to v0.36.3 ([#177](https://github.com/rknightion/genai-otel-bridge/issues/177)) ([3424b4f](https://github.com/rknightion/genai-otel-bridge/commit/3424b4f928c89a9f3cc4ca7c8327f3f5e89c4776))
* **deps:** update module github.com/grafana/pyroscope-go to v1.4.0 ([#28](https://github.com/rknightion/genai-otel-bridge/issues/28)) ([16a9272](https://github.com/rknightion/genai-otel-bridge/commit/16a927208c58f3ab9fea6e44cd679248a5b9d344))
* **deps:** update module github.com/grafana/pyroscope-go to v1.4.1 ([#157](https://github.com/rknightion/genai-otel-bridge/issues/157)) ([1a6ee1a](https://github.com/rknightion/genai-otel-bridge/commit/1a6ee1a0a8eefcef9adcdd01a3886fb3dc14ce04))
* **deps:** update module go.opentelemetry.io/proto/otlp to v1.11.0 ([#175](https://github.com/rknightion/genai-otel-bridge/issues/175)) ([f94115a](https://github.com/rknightion/genai-otel-bridge/commit/f94115a59fa663b4527d83a506fe25121b3bec6f))
* **deps:** update module k8s.io/apimachinery to v0.36.3 ([#176](https://github.com/rknightion/genai-otel-bridge/issues/176)) ([fda402e](https://github.com/rknightion/genai-otel-bridge/commit/fda402e9df4e3c9c917717579e2a50dbe06bcc78))
* **docs:** reword main.html comment so it doesn't trip the hub build guard ([7c43023](https://github.com/rknightion/genai-otel-bridge/commit/7c4302381330d41ad28c6ca976218ac03aa081b7)), closes [#26](https://github.com/rknightion/genai-otel-bridge/issues/26)
* **ecs:** build task IAM policy as a literal list, not jsondecode of a deferred data source ([60243f2](https://github.com/rknightion/genai-otel-bridge/commit/60243f26f1074ef567c8045de2430d5f70fbea0e))
* **emit:** count OTLP 200 partial_success rejects instead of silently dropping ([6ec9e1e](https://github.com/rknightion/genai-otel-bridge/commit/6ec9e1e34cea626428dc5ec95e5caa6a410d981d)), closes [#80](https://github.com/rknightion/genai-otel-bridge/issues/80)
* **emit:** never follow redirects on the OTLP emit leg ([b11b232](https://github.com/rknightion/genai-otel-bridge/commit/b11b2328197aa12b3b41747693191915ffbe08dd)), closes [#29](https://github.com/rknightion/genai-otel-bridge/issues/29)
* **guard:** harden content-egress floor and cardinality-budget correctness ([1ef0d83](https://github.com/rknightion/genai-otel-bridge/commit/1ef0d835e48f09de6d55afd9355f7d81680f5df0)), closes [#75](https://github.com/rknightion/genai-otel-bridge/issues/75) [#95](https://github.com/rknightion/genai-otel-bridge/issues/95) [#51](https://github.com/rknightion/genai-otel-bridge/issues/51) [#97](https://github.com/rknightion/genai-otel-bridge/issues/97) [#99](https://github.com/rknightion/genai-otel-bridge/issues/99)
* **guard:** scope the content denylist per-loop so an opt-in can't widen another loop's allowed set ([8e42dd8](https://github.com/rknightion/genai-otel-bridge/commit/8e42dd8a27f14f612011ac0324ca9fd4ece1a242)), closes [#130](https://github.com/rknightion/genai-otel-bridge/issues/130)
* **ha:** auto-heal Noop epoch from stored checkpoint on HA→none downgrade ([83cf358](https://github.com/rknightion/genai-otel-bridge/commit/83cf358529efaf87385415f24e4be564c56a4ce8)), closes [#45](https://github.com/rknightion/genai-otel-bridge/issues/45)
* honor Retry-After, align checkpoint RMW budgets, re-campaign on lease lapse ([52a3f3f](https://github.com/rknightion/genai-otel-bridge/commit/52a3f3f9bca169de53926c6c9ee2993065ce1d28)), closes [#122](https://github.com/rknightion/genai-otel-bridge/issues/122) [#116](https://github.com/rknightion/genai-otel-bridge/issues/116) [#110](https://github.com/rknightion/genai-otel-bridge/issues/110)
* **portkey:** correct groups metric names in signals catalogue and docs ([2b2ba3f](https://github.com/rknightion/genai-otel-bridge/commit/2b2ba3fb9b85c7f0139f81071632e3ffd18e6d52)), closes [#58](https://github.com/rknightion/genai-otel-bridge/issues/58) [#105](https://github.com/rknightion/genai-otel-bridge/issues/105)
* **portkey:** redact signed-URL from download errors and skip oversize JSONL lines ([a962763](https://github.com/rknightion/genai-otel-bridge/commit/a9627639c4802018a1ead28d4c119ccf5938900e)), closes [#34](https://github.com/rknightion/genai-otel-bridge/issues/34) [#35](https://github.com/rknightion/genai-otel-bridge/issues/35)
* **runtime:** make version observable, fix liveness/self-interval, and correct shutdown+metric docs ([5ba48d7](https://github.com/rknightion/genai-otel-bridge/commit/5ba48d7f3364aeb488ee390c9e29f7cd169c8d18)), closes [#71](https://github.com/rknightion/genai-otel-bridge/issues/71) [#73](https://github.com/rknightion/genai-otel-bridge/issues/73) [#76](https://github.com/rknightion/genai-otel-bridge/issues/76) [#88](https://github.com/rknightion/genai-otel-bridge/issues/88) [#90](https://github.com/rknightion/genai-otel-bridge/issues/90) [#91](https://github.com/rknightion/genai-otel-bridge/issues/91)
* **runtime:** widen upstream histogram, bounded ordered shutdown, and add emit/degraded instruments ([1c940e0](https://github.com/rknightion/genai-otel-bridge/commit/1c940e0842d730e3e807cfa442f75127758687f1)), closes [#121](https://github.com/rknightion/genai-otel-bridge/issues/121) [#129](https://github.com/rknightion/genai-otel-bridge/issues/129)
* **schedule:** correct snapshot catch-up, terminal-halt backoff, backfill-count re-count, and logs budget key ([f79c351](https://github.com/rknightion/genai-otel-bridge/commit/f79c3516558495442ca50ca0da64edce6221cd4a)), closes [#92](https://github.com/rknightion/genai-otel-bridge/issues/92) [#93](https://github.com/rknightion/genai-otel-bridge/issues/93) [#94](https://github.com/rknightion/genai-otel-bridge/issues/94) [#98](https://github.com/rknightion/genai-otel-bridge/issues/98)
* **seo:** clean browser-tab &lt;title&gt; (fleet cosmetic fix) ([0a390df](https://github.com/rknightion/genai-otel-bridge/commit/0a390dfec36da9688e5a321a839f4f28956ef7ae)), closes [#26](https://github.com/rknightion/genai-otel-bridge/issues/26)
* **seo:** clean homepage og:title (fleet cosmetic fix) ([59cdd5f](https://github.com/rknightion/genai-otel-bridge/commit/59cdd5f7acfa699419016a7275a83629c1f03cdb)), closes [#26](https://github.com/rknightion/genai-otel-bridge/issues/26)
* **source:** langsmith/portkey loop correctness, quota+auth taxonomy, and enum validation ([4c75e89](https://github.com/rknightion/genai-otel-bridge/commit/4c75e89c855c6c2f17246cd1bfb32117efac7d9c)), closes [#53](https://github.com/rknightion/genai-otel-bridge/issues/53) [#54](https://github.com/rknightion/genai-otel-bridge/issues/54) [#56](https://github.com/rknightion/genai-otel-bridge/issues/56) [#57](https://github.com/rknightion/genai-otel-bridge/issues/57) [#101](https://github.com/rknightion/genai-otel-bridge/issues/101) [#102](https://github.com/rknightion/genai-otel-bridge/issues/102) [#103](https://github.com/rknightion/genai-otel-bridge/issues/103) [#104](https://github.com/rknightion/genai-otel-bridge/issues/104) [#106](https://github.com/rknightion/genai-otel-bridge/issues/106)
* **source:** source registry/ownership hardening, observability, and signed-URL security ([eae19e6](https://github.com/rknightion/genai-otel-bridge/commit/eae19e6a8bb34f148a62c66d5d685aada3de08bd)), closes [#61](https://github.com/rknightion/genai-otel-bridge/issues/61) [#62](https://github.com/rknightion/genai-otel-bridge/issues/62) [#63](https://github.com/rknightion/genai-otel-bridge/issues/63) [#65](https://github.com/rknightion/genai-otel-bridge/issues/65) [#66](https://github.com/rknightion/genai-otel-bridge/issues/66) [#133](https://github.com/rknightion/genai-otel-bridge/issues/133) [#139](https://github.com/rknightion/genai-otel-bridge/issues/139) [#140](https://github.com/rknightion/genai-otel-bridge/issues/140) [#141](https://github.com/rknightion/genai-otel-bridge/issues/141)
* wire emit-latency + degraded-gauge instruments, validate-config placeholders, redact config-parse secrets ([ae52bcf](https://github.com/rknightion/genai-otel-bridge/commit/ae52bcfc14f13d0659f36163e4d4e7de045d1700)), closes [#60](https://github.com/rknightion/genai-otel-bridge/issues/60) [#111](https://github.com/rknightion/genai-otel-bridge/issues/111) [#112](https://github.com/rknightion/genai-otel-bridge/issues/112) [#120](https://github.com/rknightion/genai-otel-bridge/issues/120)


### Documentation

* **assets:** replace the social card with one for this project ([49bbb64](https://github.com/rknightion/genai-otel-bridge/commit/49bbb642ec494d82301e8431662402f5d6f9b78d))
* correct pervasive doc/comment drift against current code ([c67e422](https://github.com/rknightion/genai-otel-bridge/commit/c67e422392e6354d2d0817ba130d8fcab47fee31)), closes [#37](https://github.com/rknightion/genai-otel-bridge/issues/37) [#42](https://github.com/rknightion/genai-otel-bridge/issues/42) [#43](https://github.com/rknightion/genai-otel-bridge/issues/43) [#44](https://github.com/rknightion/genai-otel-bridge/issues/44) [#48](https://github.com/rknightion/genai-otel-bridge/issues/48) [#55](https://github.com/rknightion/genai-otel-bridge/issues/55) [#68](https://github.com/rknightion/genai-otel-bridge/issues/68) [#77](https://github.com/rknightion/genai-otel-bridge/issues/77) [#78](https://github.com/rknightion/genai-otel-bridge/issues/78) [#79](https://github.com/rknightion/genai-otel-bridge/issues/79) [#83](https://github.com/rknightion/genai-otel-bridge/issues/83) [#89](https://github.com/rknightion/genai-otel-bridge/issues/89) [#100](https://github.com/rknightion/genai-otel-bridge/issues/100)
* fix config-accuracy errors found in final review ([62ac033](https://github.com/rknightion/genai-otel-bridge/commit/62ac03333c0e9ff8860f6b626253b86ff13fe465))
* **geo:** content-shape pass for LLM/search retrievability ([9eb656f](https://github.com/rknightion/genai-otel-bridge/commit/9eb656fea06b14deed59427d62f8b7629ba80dfc))
* scaffold zensical site + dispatch workflow for m7kni.io hub ([2736648](https://github.com/rknightion/genai-otel-bridge/commit/2736648ebe67a628cf8b969964c0f307b67b1633))
* write full user-documentation page set ([79c83a9](https://github.com/rknightion/genai-otel-bridge/commit/79c83a9e01d61c6014e752c786dea3d652f76416))


### Build & CI

* add hadolint + trivy Docker security scans ([2c8dc84](https://github.com/rknightion/genai-otel-bridge/commit/2c8dc840536ecf13c64a44b18a4a249498328c74))
* add OpenSSF Scorecard via shared reusable workflow ([8578368](https://github.com/rknightion/genai-otel-bridge/commit/8578368526873463c6c27f41f572da00182f48fb))
* add Snyk -&gt; Snyk Cloud monitor (SCA/SAST/IaC/container) ([5113e36](https://github.com/rknightion/genai-otel-bridge/commit/5113e36029c7ac8c96ad94d0f954ec05d74ed2a7))
* adopt shared rknightion/.github reusable security workflows ([25fbdb3](https://github.com/rknightion/genai-otel-bridge/commit/25fbdb3a9a413707bc35ef164534c384f658927c))
* auto-assign maintainer on new issues (notify by email) ([a919c0f](https://github.com/rknightion/genai-otel-bridge/commit/a919c0f0c2c3828a531974c5b5837b39eb117c61))
* build + publish edge :main image + snapshot chart on push to main ([3cbf760](https://github.com/rknightion/genai-otel-bridge/commit/3cbf7605a19a8dd9cecbdbe9f25640ee2cc3de21))
* bump shared rknightion reusables to v1.3.1 ([ecaab65](https://github.com/rknightion/genai-otel-bridge/commit/ecaab6537b543bc7789f7d3048fcb79bc71fa922))
* **codacy:** add local analysis config + Cloud file exclusions ([2959c88](https://github.com/rknightion/genai-otel-bridge/commit/2959c883a012f7e85b45c6f3a2ddd45544d3c5b0))
* drop CodeQL pull_request trigger to trim Actions fan-out ([8e6fcd9](https://github.com/rknightion/genai-otel-bridge/commit/8e6fcd9dc341f5d658b361cec434a2d64a29d72b))
* force module mode and align make-gate lint with CI ([db30ea0](https://github.com/rknightion/genai-otel-bridge/commit/db30ea090c371954cf0d79fc8ce4932c20926097))
* grant checks:read to release-please's publish call to fix startup_failure ([ef6c318](https://github.com/rknightion/genai-otel-bridge/commit/ef6c3184c5aa99b2e14451658982b1f4ade27250)), closes [#149](https://github.com/rknightion/genai-otel-bridge/issues/149)
* open Renovate PRs by counting internal checks as success ([91c65f2](https://github.com/rknightion/genai-otel-bridge/commit/91c65f20591297eda192535a8efe9f3f4797ce22))
* open the release-please PR under a PAT so CI runs without manual approval ([f85574d](https://github.com/rknightion/genai-otel-bridge/commit/f85574d00933cf442ff2f62e46a5dff606f4646f))
* pin shared rknightion reusables to v1.0.0 ([ba573e7](https://github.com/rknightion/genai-otel-bridge/commit/ba573e748a7b7afae7bec9e159a3d8a9fe278403))
* publish image + Helm chart via shared container-publish reusable ([5e59ce5](https://github.com/rknightion/genai-otel-bridge/commit/5e59ce5098a2981baac7b79d2822846aa7646a3a))
* reference rknightion/.github reusables [@main](https://github.com/main) (unpin from digest) ([302ec29](https://github.com/rknightion/genai-otel-bridge/commit/302ec2923f9a1e811b3025ab74f0b080628e61f4))
* remove claude issue-triage workflow ([a465421](https://github.com/rknightion/genai-otel-bridge/commit/a46542173af9e5ce827643d2a9786ed7bc7b485a))
* remove hygiene-fork-backstop (pull_request_target) workflow ([f969458](https://github.com/rknightion/genai-otel-bridge/commit/f96945828b5fd3030ea32aee9028a31d6a7a5bad))
* remove notify-maintainer-on-new-issue workflow ([4470235](https://github.com/rknightion/genai-otel-bridge/commit/4470235cc3ab5eb59bbc7d427b4a9612cd8b41ba))
* **renovate:** remove local pr limits + minimumReleaseAge pin ([ba1c3b3](https://github.com/rknightion/genai-otel-bridge/commit/ba1c3b337c90708ecdeb655001703fde1ce0edcf))
* report Go test coverage to Codacy ([a90ede4](https://github.com/rknightion/genai-otel-bridge/commit/a90ede4d506b77b3acba4687b0966cd4fff4489e))
* resolve actionlint/shellcheck + zizmor workflow findings ([437450d](https://github.com/rknightion/genai-otel-bridge/commit/437450d475907481212110fc57a9228bd2f1921a))
* sha256-verify third-party tool downloads and add a fork-PR hygiene backstop ([4872de5](https://github.com/rknightion/genai-otel-bridge/commit/4872de5215539991c32195f2cc4e763b241a8b5f)), closes [#59](https://github.com/rknightion/genai-otel-bridge/issues/59) [#108](https://github.com/rknightion/genai-otel-bridge/issues/108)
* use Codacy account token for coverage upload ([2b02c4d](https://github.com/rknightion/genai-otel-bridge/commit/2b02c4da016f1e39cb2696a1dcce71e30ea12536))

## [3.0.1](https://github.com/rknightion/genai-otel-bridge/compare/v3.0.0...v3.0.1) (2026-06-26)


### Bug Fixes

* force release-please generic updater on Chart.yaml; add workflow_dispatch ([3050d0b](https://github.com/rknightion/genai-otel-bridge/commit/3050d0bda9c1a104600bc85f741ecb8d892d7481))


### Documentation

* repo is public + main requires ci-success (admin bypass) ([f2b88e2](https://github.com/rknightion/genai-otel-bridge/commit/f2b88e24e90df60af064a037629452ad254c12dd))


### Build & CI

* add hybrid issue-triage (no-tools AI analysis + deterministic apply) ([98cc6a0](https://github.com/rknightion/genai-otel-bridge/commit/98cc6a0dcf2f23883eaeee51cb3e74059b5805ca))
* automate releases with release-please (changelog + GitHub Releases + chart bump) ([d5c84f1](https://github.com/rknightion/genai-otel-bridge/commit/d5c84f1d6f563aeb16a6a3bdd2c2c1338f06642c))
* parallelize CI matrix + add ci-success gate; enable Renovate automerge ([fd6a293](https://github.com/rknightion/genai-otel-bridge/commit/fd6a29352e7c3bf39e04dd2c20f7042433977a63))
* wire release-please config, workflows, and chart bump ([98db168](https://github.com/rknightion/genai-otel-bridge/commit/98db1681116894919db0d4530df5d7e05d1b5f8a))

## [3.0.0] - 2026-06-26

### Build & CI
- Harden GitHub Actions workflow (zizmor)
- Enforce the forbidden-words guard in CI via FORBIDDEN_WORDS_PATTERN
- Strengthen leak detection — credential shapes, gitleaks, fix silent grep bug

### Refactor
- Unify project naming as genai-otel-bridge (retire "decant")
## [2.1.1] - 2026-06-25

### Bug Fixes
- Empty-array-safe PRIVATE_NAMES expansion (bash 3.2 + set -u)
- Example image -> ghcr.io/rknightion/genai-otel-bridge:latest (GHCR has no :main)
- Log in to the registry host, not host/namespace (GHCR support)
## [2.1.0] - 2026-06-25

### Build & CI
- Disable tag-triggered auto-promotion (freeze customer promotion during rename)
- Narrow .github exclude to workflows/ so issue+PR templates promote
- Publish image + chart to GHCR from the public repo on v* tags

### Documentation
- Add OSS governance files + README quickstart
- Sanitize root CLAUDE.md + put CLAUDE.md on the public surface

### Features
- Add 'genai-otel-bridge -validate-config' for secret-free config/overlay validation

### Refactor
- Rename make-aip-oi-secret.sh -> make-genai-otel-bridge-secret.sh
- Genericize EKS example, move onto the public surface
- Extract customer delivery artifacts out of the product repo
## [2.0.0] - 2026-06-25

### Dependencies
- Optimise renovate config for vendored go + conventional commits

### Refactor
- Rename project to genai-otel-bridge, artifacts to genai-otel-bridge
## [1.5.0] - 2026-06-24

### Bug Fixes
- Correct alert state enum OK -> Ok

### Documentation
- Correct GS1 promote-lists to live indexed sets
- Document v2 dashboard, dynamic thresholds, 11-alert set
- Note langsmith product rules + per-bucket-gauge staleness gotcha
- Record late-data investigation (§11) + revision-age instrumentation

### Features
- Add self-relative staleness recording rules
- Self-relative PollerStale + 7 new self-obs alerts
- Rebuild self-obs dashboard as v2 tabs + dynamic layout
- Add LangSmith product recording rules
- Instrument bucket-revision lateness (age histogram)
- Add bucket-revision lateness panel (age p50/p95)
- Map native top-level trace_id field to OTLP trace_id
## [1.4.0] - 2026-06-24

### Bug Fixes
- Gofmt config.go alignment + test stamper functions to satisfy unused lint

### Documentation
- Document portkey api_key_use_cases per-key labelling
- Tighten api_key_use_case slug-rule wording (run-collapse)

### Features
- Lift metadata correlation_id into logs (record attr + OTLP trace_id)
- Add api_key_use_cases to Portkey SourceConfig
- SlugifyUseCase label normaliser
- ResolveUseCases validation + use-case stampers
- Allow-list api_key_use_case (+ app golden union)
- Analytics N internal filtered passes + api_key_use_case stamp
- Groups N internal filtered passes + api_key_use_case stamp
- Logs_export per-use-case fan-out + record-tier api_key_use_case
- Surface portkey api_key_use_cases example

### Testing
- Analytics use-case stamp + api_key_ids filter e2e
- Logs export api_key_ids filter body + record stamp
- ValidateOwnership passes with api_key_use_cases (M7 regression guard)
# Changelog

All notable changes to genai-otel-bridge. Subsequent releases are generated from Conventional Commits.

## [1.3.1] - 2026-06-23

### Bug Fixes

- Track released version in appVersion + auto-stamp on changelog

## [1.3.0] - 2026-06-23

### Features

- Notebook-parity analytics — token_type split, latency avg, api_key_ids filter

## [1.2.3] - 2026-06-23

### Bug Fixes

- Rebuild wrong-arch cached tools-e2e binaries (helm/k3d/kubectl)

## [1.2.2] - 2026-06-23

### Bug Fixes

- Pin gate jobs to arm64 + rebuild wrong-arch cached tools

## [1.2.1] - 2026-06-23

### Bug Fixes

- Record last_success on the logs emit path (health coverage for logs loops)
- Pull golang builder base via mirror.gcr.io to dodge Docker Hub rate limits

## [1.2.0] - 2026-06-23

### Bug Fixes

- Allow product OTLP egress to in-cluster Alloy on 4318
- Make self-obs health stats self-contained (not recording-rule dependent)
- Correct scrape_healthy bool + add poller-down/auth self-obs alerts

### Documentation

- Document the self-obs poller-health alerts

### Features

- Add genai-otel-bridge self-observability dashboard (v2, self-obs role)
- Stamp a `source` record attribute on product logs (portkey/langsmith)

## [1.1.1] - 2026-06-23

### Bug Fixes

- Recover logs_export from a lost-ack /start instead of wedging on AB01
- Deploy poller into the grafana-poller namespace (ESO SA alignment)

### Documentation

- Align dev overlay with renamed langsmith-poller secret
- Consume existing shared portkey/langsmith secrets

### Features

- Route product telemetry through in-cluster Alloy; enable self traces + profiles

## [1.1.0] - 2026-06-23

### Features

- Add gated ESO SecretStore/ExternalSecret + runtimeEnv to deploy/helm

## [1.0.3] - 2026-06-23

### Build & CI

- **Scope the deploy jobs to the `dev` environment** so the environment-scoped registry/OIDC
  configuration resolves at build time (the image build/push + GitOps sync jobs now declare
  `environment: dev`, matching the rest of the estate).

## [1.0.2] - 2026-06-23

### Build & CI

- **Parallelised the CI gate** — its steps (build, vet, lint, unit test, race, acceptance, envtest,
  helm-lint) now run as concurrent jobs rather than one sequential run, cutting wall-clock to roughly the
  slowest single step. All steps still block the image publish.

## [1.0.1] - 2026-06-23

### Build & CI

- **Delivery pipeline** — the CI now builds the container image and pushes it to the configured
  registry, then syncs the Helm chart into the GitOps deployment repo and pins the released image tag.
- **Kaniko build path** (`Dockerfile.kaniko`) — a BuildKit-free Dockerfile for daemonless CI builders
  (native single-arch build).
- The image is tagged with both the deployment-tracking tag and the `vX.Y.Z` release tag.

## [1.0.0] - 2026-06-23

Initial release.

genai-otel-bridge is a vendor-neutral integrator that polls AI-platform APIs (LLM gateways such as Portkey,
evaluation platforms such as LangSmith) and emits operational telemetry to Grafana Cloud as OTLP
metrics and logs.

### Features

- **Portkey source** — `analytics` + `groups` → OTLP metrics; `logs_export` → OTLP logs.
- **LangSmith source** — `sessions`/eval → OTLP metrics; `runs` → a content-free OTLP log index.
- **Content-free by design** — never requests prompt/response bodies; an outbound field allow/deny-list
  governs every emitted field, enforced as a release gate.
- **Highly available** — leader-elected single-emit; monotonic, lease-epoch-fenced checkpointing;
  conditional gap-free emit within source retention plus the sink accept window.
- **Deterministic OTLP** — hand-encoded, byte-identical re-emit (the precondition for sink idempotency).
- **Operationally honest** — every polling/emit gap or skipped sample is an alertable signal, never silent.
- **Configurable governance** — content allow/deny-list and per-metric cardinality guard; durability
  tuning sized to the Grafana Cloud out-of-order / too-old accept windows.
- **Self-observing** — own metrics + logs (distinct resource identity), optional self-profiling and
  self-tracing.
- **Hardened Helm chart** — non-root, default-deny network policy, PDB, least-privilege RBAC.

Licensed under AGPL-3.0-only.
