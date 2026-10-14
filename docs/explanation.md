# wallet-go — material de divulgação e entrevista

Este arquivo não é documentação técnica do projeto — para isso, ver `README.md`, os
ADRs em `docs/adr/`, `docs/performance-report.md` e `docs/resilience-report.md`. É
material de apoio pessoal: um roteiro pra responder "me conta sobre um projeto em Go
que você fez" sem travar, e a base pra qualquer corte menor (post de LinkedIn, bullet
de currículo, resposta de 30 segundos numa entrevista).

## O que é

Uma API de carteira digital em Go onde o saldo **nunca é uma coluna**. Toda movimentação
de dinheiro é um lançamento numa tabela de partidas dobradas (double-entry), nunca
editado nem apagado — o saldo de uma conta é sempre a soma dos seus lançamentos,
calculada na hora. Transferências entre contas são atômicas e, sob concorrência real
(duas transferências simultâneas na mesma conta), continuam corretas.

## Objetivo

O projeto não existe pra provar que eu sei fazer um CRUD de transferência — isso é
trivial. Ele existe pra provar quatro coisas específicas: que eu sei representar
dinheiro sem perder centavo; que um retry de rede (cliente reenviando a mesma
requisição por timeout) nunca duplica uma transferência; que uma invariante de saldo se
mantém sob concorrência de verdade, não só no caminho feliz; e que uma decisão de
arquitetura (qual estratégia de concorrência usar) pode ser tomada por medição, em vez
de preferência. É um projeto pensado pra ser defendido verbalmente numa entrevista, sem
precisar abrir o código pra lembrar por que cada decisão foi tomada.

## Métodos

**Modelo de dados.** Ledger append-only com partidas dobradas: toda transferência gera
duas linhas que somam exatamente zero. Duas invariantes são verificadas automaticamente
no CI: nenhuma transferência pode somar diferente de zero, e nenhuma conta de cliente
pode ficar negativa (só a conta `system`, contraparte de depósitos e saques, pode).

**Idempotência.** Cada chave de idempotência é escopada por `(dono da chave, endpoint,
chave)` — nunca global — e acompanhada de um fingerprint SHA-256 do corpo da requisição
canonicalizado. A reivindicação da chave, a execução da transferência e o armazenamento
da resposta acontecem na mesma transação SQL: ou os três acontecem juntos, ou nenhum
acontece.

**Concorrência, comparada por medição.** Duas estratégias de transferência atrás da
mesma interface, trocáveis por variável de ambiente, sem mudar uma linha do caso de
uso: locking pessimista (`SELECT FOR UPDATE` nas duas contas, sempre em ordem crescente
de id, pra transferências em sentidos opostos nunca travarem uma esperando a outra) e
`SERIALIZABLE` otimista, com retry e backoff quando o Postgres detecta conflito.
Construí um gerador de carga próprio (não usei ferramenta de terceiros) pra rodar
cenários de alta e baixa contenção, com seed determinístico, e medir throughput,
p50/p95/p99 e taxa de retry de cada estratégia sob carga real.

**Otimização guiada por dado, não por intuição.** Um code review identificou que uma
única transferência fazia oito idas-e-voltas sequenciais ao banco. Profiling de CPU
(`pprof`) confirmou: 91% do tempo não era computação, era a goroutine esperando a rede.
Reescrevi o caminho de transferência pra agrupar as queries em dois lotes pipelinados
em vez de oito chamadas sequenciais — sem mudar a ordem de execução no servidor, então a
garantia contra deadlock não foi tocada — e medi de novo pra confirmar o ganho antes de
aceitar a mudança.

**Segurança operacional.** O processo da aplicação conecta ao banco com um papel de
privilégio mínimo, sem permissão de `UPDATE` ou `DELETE` em lançamentos — a regra de
"ledger nunca é editado" é garantida pelo próprio Postgres, não só pela disciplina do
código. O contêiner roda com sistema de arquivos somente leitura e sem capabilities do
Linux.

**Resiliência testada, não assumida.** Pra cada cenário de falha — queda do Postgres,
`SIGTERM` sob carga, `kill -9` no meio de uma transferência — escrevi a hipótese
**antes** de rodar o teste, e registrei o que de fato aconteceu, incluindo onde a
hipótese errou. Em todos os cenários, o ledger permaneceu correto, provado no nível de
cada chave de idempotência (uma transação morta não deixa nem o próprio registro da
tentativa, porque claim e execução são a mesma transação).

**Rate limiting por chave de API, não por IP.** Um token bucket (`golang.org/x/time/rate`)
limita cada chave de API a uma taxa configurável, por padrão 50 requisições/s com um
pico de 100 — ativado por padrão, seguindo a mesma postura de "seguro por padrão" da
segurança operacional. A decisão de ser por chave, e não por IP ou global, reaproveita
o modelo de identidade que o projeto já tinha pra idempotência, em vez de criar um
conceito novo e não-limitado (IP) só pra essa feature.

## Tecnologias

Go, chi, `pgx/v5` com connection pooling, PostgreSQL 16, Docker e Docker Compose,
`testcontainers-go` (testes de integração contra banco real, não mocks), `pprof` e
`go test -bench -benchmem`, `golangci-lint`, GitHub Actions, `golang.org/x/time/rate`
(token bucket), e um harness de carga e um
perfilador de falhas escritos à mão, em Go.

## Resultados

- **Locking sustentou cerca de 3x mais throughput que `SERIALIZABLE`** no cenário de
  baixa contenção — o oposto do que eu esperava antes de medir, já que esperava a
  diferença maior justo na alta contenção. A explicação: `SERIALIZABLE` paga um custo
  fixo de rastreamento em toda transação, mesmo sem conflito nenhum; `locking` só paga
  custo quando realmente há disputa.
- **A otimização de round trips cortou o p99 de 74ms pra 25ms**, com o mesmo throughput
  ou melhor — confirmado antes e depois, não só no código, no número.
- **Um experimento isolado (desligar a confirmação síncrona de commit, só pra medir,
  nunca em produção) quase dobrou o throughput do cenário de maior contenção**,
  confirmando que boa parte do limite observado era o tempo de confirmação em disco,
  não só a trava da linha.
- **Zero alocação e nanossegundos por operação** no caminho quente do domínio
  (validação de valor, cálculo de saldo) — a lógica de negócio em si nunca é o
  gargalo.
- **4 de 6 cenários de falha induzida testados, ledger correto em 100% deles.**

Duas filas inteiras de "extras" (uma minha, outra inspirada num desafio de vaga
diferente) ficaram deliberadamente de fora — documentado como decisão de escopo, não
lacuna esquecida. Isso também é parte da resposta, se alguém perguntar "por que você
parou aqui".
