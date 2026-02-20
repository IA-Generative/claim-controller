# Changelog

## [0.3.0](https://github.com/IA-Generative/claim-controller/compare/v0.2.0...v0.3.0) (2026-02-20)


### Features

* add common template value rendering function in _helpers.tpl ([2d79414](https://github.com/IA-Generative/claim-controller/commit/2d794142fbe480c7aa060ceb4e652299ab3e655d))
* add readiness and liveness probes to workload configuration ([17d6178](https://github.com/IA-Generative/claim-controller/commit/17d617871586de357f38ef72daa31cd66ae64065))
* implement claim readiness evaluation and status updates ([17d6178](https://github.com/IA-Generative/claim-controller/commit/17d617871586de357f38ef72daa31cd66ae64065))


### Bug Fixes

* update LoadResourceTemplate functions to include namespace param… ([#10](https://github.com/IA-Generative/claim-controller/issues/10)) ([17d6178](https://github.com/IA-Generative/claim-controller/commit/17d617871586de357f38ef72daa31cd66ae64065))
* update LoadResourceTemplate functions to include namespace parameter ([17d6178](https://github.com/IA-Generative/claim-controller/commit/17d617871586de357f38ef72daa31cd66ae64065))
* update Role apiGroups to allow all groups in rbac.yaml ([17d6178](https://github.com/IA-Generative/claim-controller/commit/17d617871586de357f38ef72daa31cd66ae64065))

## [0.2.0](https://github.com/IA-Generative/claim-controller/compare/v0.1.1...v0.2.0) (2026-02-19)


### Features

* add owner reference handling in ConfigMap and File providers ([e765d9e](https://github.com/IA-Generative/claim-controller/commit/e765d9e6612ebd8d212bb8b0dedb89e46eb56947))


### Bug Fixes

* streamline version retrieval in Chart.yaml update step ([a81c8f1](https://github.com/IA-Generative/claim-controller/commit/a81c8f141f2cd9e6475a4e0bd3b56d7679daf7c4))
* update release endpoint to accept claim ID as a path parameter ([2fd4414](https://github.com/IA-Generative/claim-controller/commit/2fd4414588a32368d361f2572f0c9fe33d195bee))

## [0.1.1](https://github.com/IA-Generative/claim-controller/compare/v0.1.0...v0.1.1) (2026-02-19)


### Bug Fixes

* update image tag handling in deployment.yaml for better versioning ([#6](https://github.com/IA-Generative/claim-controller/issues/6)) ([3e10f51](https://github.com/IA-Generative/claim-controller/commit/3e10f51a3be245e75c0876c0ef16505bb84c3040))

## [0.1.0](https://github.com/IA-Generative/claim-controller/compare/v0.0.1...v0.1.0) (2026-02-18)


### Features

* Implement claim release functionality and cleanup expired claims ([5eaa6a5](https://github.com/IA-Generative/claim-controller/commit/5eaa6a5d708c08f681e579f9ffcd3a0029f63874))
