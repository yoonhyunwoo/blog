---
title: "인시던트는 어떻게 관리되어야 하는가"
date: 2025-11-06T10:09:00Z
tags:
  - network
  - linux
summary: "리눅스 커널의 네트워크 스택은 복잡하고 안정적이지만 그렇기에 특정한 지점에서 병목이 발생할 수 있습니다. 본 글은 커널 수신 경로(L2~L4)의 처리 과정과 주요 병목 지점을 분석하고, 단계별 튜닝 방법과 핵심 파라미터를 설명합니다."
description: "NAPI 기반 수신 구조, IP 라우팅 및 검증, TCP 연결 관리 등 커널 내부 흐름을 다루며, Red Hat Performance Tuned와 커널 문서를 기반으로 작성되었습니다."
---

요즘들어 Incident(사건, 사고)라는 개념이 자주 대두되고 있습니다. 여러 SRE(사이트 신뢰성 엔지니어링) 조직에서는 일종의 인시던트가 발생할 때마다 각자의 방식으로 대처하고 있습니다. De fecto에 가까운 서비스가 없기에 이를 구현하는 방법은 직접 구현하는 방법과 여러 SaaS 플랫폼을 이용하는 방식이 있습니다.

직접 구현하는 사례는 국내에서는 대표적으로 아래와 같은 사례들이 있습니다.
- [올리브영은 인시던트를 어떻게 관리하고 있는가?](https://oliveyoung.tech/2024-01-23/incident/)
- [ChatOps를 통한 업무 자동화(feat. Slack Hubot)](https://techblog.lycorp.co.jp/ko/how-to-use-chatops-to-automate-devops-tasks-feat-slack-hubot)

또는 이미 사용되고 있는 모니터링 플랫폼, 채팅 플랫폼이나 전용 SaaS 제품군등에 통합되어 나오는 경우가 있습니다.
- incident.io
- Datadog
- Jira
- etc...

인시던트란 