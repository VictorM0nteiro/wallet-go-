# wallet-go — Carteira digital com ledger append-only

> Documento de escopo. Serve de briefing para as sessões de trabalho no projeto.

---

## 1. Objetivo

### O que o sistema é

Uma API de carteira digital onde o saldo nunca é uma coluna: é derivado de um ledger
append-only, e transferências entre contas são idempotentes e corretas sob concorrência.

Essa frase vai no topo do README. Tudo que não a sustenta está fora do escopo.

### O que é um ledger

Ledger é o **registro imutável e ordenado de toda movimentação de valor do sistema**. Ele
é a fonte única da verdade financeira: qualquer número que o sistema informe sobre dinheiro
tem que ser derivável dele.

Três propriedades definem o modelo:

**1. Cada movimentação registra os dois lados.** Dinheiro não aparece nem desaparece — ele
sai de algum lugar e entra em outro. Uma transferência de R$ 50 de Ana para Bruno não é um
registro, são dois:

| Conta | amount_cents |
|---|---|
| Ana | −5.000 |
| Bruno | +5.000 |
| **Soma** | **0** |

É a versão mínima da ideia contábil de partidas dobradas. A soma zero vira uma invariante
testável: se algum dia uma transferência não somar zero, existe um bug de criação de
dinheiro, e o teste falha antes de o bug chegar em produção.

**2. Nada é editado ou apagado.** Se um valor está errado, você não corrige a linha — você
acrescenta um lançamento de estorno que a anula. O passado é imutável, e por isso você
nunca sabe menos sobre ele com o tempo. Esse é o "append-only" do nome.

**3. Saldo não é armazenado, é concluído.** O saldo de uma conta é a soma de todos os
lançamentos dela. A analogia útil é o seu extrato bancário: o extrato é o fato, o saldo é
uma conclusão tirada dele. O ledger guarda o extrato e calcula o saldo, nunca o contrário.

### Por que não a alternativa óbvia

O modelo ingênuo é uma coluna `balance` atualizada com `UPDATE contas SET balance =
balance - 50`. É mais simples e é o que a maioria dos projetos de portfólio faz. Os
problemas aparecem em produção, não no `docker compose`:

- **Não existe histórico.** Se o saldo estiver errado, não há como descobrir quando nem por
  quê. Você tem um número, não uma explicação.
- **A divergência é silenciosa.** Um bug que perde um `UPDATE` deixa o saldo errado para
  sempre e ninguém é notificado. No ledger, o mesmo bug quebra a invariante de soma zero e
  aparece.
- **Auditoria é impossível.** Nenhum sistema que lida com dinheiro de terceiros sobrevive
  sem conseguir responder "de onde veio esse valor".

O preço do ledger é real e está assumido: a leitura de saldo fica mais cara, porque vira
uma agregação sobre N lançamentos em vez de ler uma coluna. Esse custo é conhecido, está
documentado no ADR 1, e sua mitigação (snapshot incremental) está na fila de extras.

### O que o projeto prova

Que você sabe representar dinheiro sem perder centavo, garantir que um retry não transfira
duas vezes, manter uma invariante sob concorrência real, e medir uma decisão de engenharia
em vez de argumentar por gosto.

---

## 2. Escopo

### Dentro

- Criar conta, depósito, saque, transferência entre contas
- Consulta de saldo derivado do ledger
- Extrato paginado
- `Idempotency-Key` obrigatório em toda escrita, com replay e detecção de conflito
- Invariante de saldo não-negativo garantida sob concorrência
- Comparação medida de duas estratégias de concorrência

### Fora (deliberadamente)

- Usuários, KYC, autenticação além de uma API key estática
- Multi-moeda, juros, agendamento, transferência futura
- Frontend de qualquer tipo
- Mensageria, microsserviços, CQRS, event sourcing
- Integração com qualquer sistema de pagamento real

**Regra de escopo:** diante da dúvida "adiciono uma feature ou aprofundo o que existe",
aprofunde. Seis endpoints medidos e documentados valem mais que vinte endpoints rodando
só no seu Docker Compose.

---

## 3. Modelo de dados

```sql
CREATE TABLE accounts (
    id          UUID PRIMARY KEY,
    owner_id    TEXT NOT NULL,
    kind        TEXT NOT NULL,          -- 'customer' | 'system'
    currency    CHAR(3) NOT NULL DEFAULT 'BRL',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_id, kind, currency)
);

CREATE TABLE transfers (
    id               UUID PRIMARY KEY,
    from_account_id  UUID NOT NULL REFERENCES accounts(id),
    to_account_id    UUID NOT NULL REFERENCES accounts(id),
    amount_cents     BIGINT NOT NULL CHECK (amount_cents > 0),
    status           TEXT   NOT NULL,   -- 'completed' | 'rejected'
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_account_id <> to_account_id)
);

CREATE TABLE entries (
    id           BIGSERIAL PRIMARY KEY,
    transfer_id  UUID   NOT NULL REFERENCES transfers(id),
    account_id   UUID   NOT NULL REFERENCES accounts(id),
    amount_cents BIGINT NOT NULL CHECK (amount_cents <> 0),  -- + entra, − sai
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX entries_account_idx ON entries (account_id, id);

CREATE TABLE idempotency_keys (
    scope                TEXT NOT NULL,      -- dono da chave
    endpoint             TEXT NOT NULL,
    key                  TEXT NOT NULL,
    request_fingerprint  TEXT NOT NULL,      -- sha256 do corpo canonicalizado
    state                TEXT NOT NULL,      -- 'in_flight' | 'completed'
    status_code          INT,
    response_body        JSONB,
    transfer_id          UUID REFERENCES transfers(id),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, endpoint, key)
);
```

### A conta `system`

Depósito e saque quebrariam a invariante de soma zero se não tivessem contraparte.
Existe **uma** conta de `kind = 'system'`: depositar é transferir de `system` para o
cliente, sacar é o inverso. Ela é a única autorizada a ficar com saldo negativo — é a
única exceção do sistema e está documentada como tal.

### Invariantes, ambas testadas no CI

```sql
-- 1. todo transfer soma zero
SELECT transfer_id, SUM(amount_cents) FROM entries
GROUP BY transfer_id HAVING SUM(amount_cents) <> 0;

-- 2. nenhuma conta de cliente fica negativa
SELECT e.account_id, SUM(e.amount_cents) FROM entries e
JOIN accounts a ON a.id = e.account_id
WHERE a.kind = 'customer'
GROUP BY e.account_id HAVING SUM(e.amount_cents) < 0;
```

Ambas devem retornar zero linhas, sempre. É a proteção mais barata contra a classe de
bug mais cara.

---

## 4. Decisões já travadas

**Dinheiro.** `int64` de centavos no Go, `BIGINT` no Postgres. Nunca float, em lugar
nenhum, nem em log, nem em JSON de resposta.

**Ordem de aquisição de lock.** Transferência toca duas contas. Duas transferências
simultâneas em sentidos opostos (A→B e B→A) causam deadlock se cada uma travar a sua
conta de origem primeiro. Regra: **sempre travar em ordem crescente de `account_id`**,
independentemente de quem é origem e quem é destino.

**Contrato de idempotência.**

| Situação | Resposta |
|---|---|
| Chave nova | Processa, grava resposta, devolve |
| Chave repetida, corpo idêntico | Devolve a resposta armazenada, mesmo status |
| Chave repetida, corpo diferente | `422` |
| Chave em `in_flight` | `409` |

Chave escopada por `(scope, endpoint, key)` — nunca global.

**Saldo é derivado.** `SUM(amount_cents)` sobre as entries da conta. Sem coluna de saldo,
sem `UPDATE` de saldo. O custo dessa escolha é conhecido e vira a fila de extras.

---

## 5. Arquitetura

Hexagonal, na versão mínima. A razão é prática: o caso de uso precisa não saber qual
estratégia de concorrência está em uso, senão a medição da fase C compara três versões
diferentes do sistema em vez de três estratégias.

```
cmd/wallet/            binário
internal/
  domain/              Money, Account, Transfer, invariantes, erros tipados
                       — zero import de pgx, net/http ou qualquer infra
  app/                 caso de uso Transferir + as portas (interfaces)
  adapters/
    http/              handlers, DTOs, tradução erro de domínio → status HTTP
    postgres/          repositórios + implementações da porta de transferência
```

A porta de transferência é uma interface de um método, implementada duas vezes no adapter
Postgres, escolhida por variável de ambiente.

**Teste de que está certo:** `go test ./internal/domain/...` roda em milissegundos, sem
Docker. Se testar a regra de saldo não-negativo exigir container, a hexagonal virou
decoração.

### Endpoints

```
POST /accounts
POST /accounts/{id}/deposits
POST /accounts/{id}/withdrawals
POST /transfers
GET  /accounts/{id}/balance
GET  /accounts/{id}/entries        (paginado por cursor)
```

---

## 6. Fases

Orçamento: ~34h, 14 semanas.

### Fase A — núcleo (semanas 1-3)

- Estrutura hexagonal, CI no GitHub Actions com `golangci-lint` + `go test -race` desde o dia 1
- Migrations versionadas (`golang-migrate` ou `goose`)
- Domínio: `Money`, `Account`, `Transfer`, invariantes, erros tipados — testado sem banco
- Adapter Postgres com pgx, testcontainers-go, transferência funcionando ponta a ponta

**Pronto quando:** `go test -race ./...` passa e as duas invariantes de seção 3 estão
cobertas por teste.

### Fase B — apresentável (semanas 4-6)

- Idempotência completa conforme a tabela da seção 4
- Teste de concorrência: N requisições simultâneas com a **mesma** chave → exatamente um
  transfer e um par de entries
- Os seis endpoints, com `context` propagado do handler à query e timeouts explícitos
- `log/slog` estruturado com correlation ID por requisição
- Graceful shutdown: para de aceitar conexões, drena as em voo, fecha o pool, sai — com timeout
- `docker compose up` sobe tudo; README v1

**Pronto quando:** outra pessoa clona, sobe e faz uma transferência sem te perguntar nada.

Este é o ponto de corte. Se um processo seletivo abrir aqui, o projeto já é apresentável.

### Fase C — medição (semanas 7-9)

- Estratégia 1: `SELECT ... FOR UPDATE` nas contas, em ordem de `account_id`
- Estratégia 2: isolamento `SERIALIZABLE` com retry no erro de serialização
- Harness de carga próprio em Go, dois cenários de contenção
- Coleta de métricas e perfil com pprof

### Fase D — documentação (semanas 10-12)

- Relatório de medição com metodologia
- 4 ADRs
- README final com a seção de limitações conhecidas
- Folga para o atraso acumulado das fases A e B

### Fase E — falha induzida (semanas 13-14)

Exige o harness da fase C: falha injetada sem carga rodando e sem métrica não demonstra
nada. Executado à mão, não automatizado — um script de chaos no CI é projeto à parte e
entrega o mesmo sinal por muito mais custo.

**O protocolo:** cada falha vira uma hipótese **escrita antes**, o comportamento observado
depois, e a divergência entre as duas. A divergência é o produto — é ela que prova que o
teste foi real.

| # | Falha injetada | Como | Hipótese a escrever antes |
|---|---|---|---|
| 1 | Postgres cai durante carga | `docker stop postgres` | Falha rápida com 503, `/readyz` vermelho, `/healthz` verde, nenhum lançamento parcial |
| 2 | Postgres volta | `docker start postgres` | Pool reconecta sozinho, sem restart do serviço, invariantes intactas |
| 3 | SIGTERM com requisições em voo | `docker stop` no app sob carga | Para de aceitar conexões, drena as em voo, fecha o pool, sai dentro do timeout |
| 4 | Latência artificial no banco | `tc netem` ou proxy lento | Timeout dispara no tempo previsto em vez de pendurar; p99 sobe sem empilhar goroutines |
| 5 | `kill -9` no meio de uma transferência | Matar o processo sob carga | Transação não commitada é descartada pelo Postgres; nenhum débito sem crédito |
| 6 | API de cotação fora | Derrubar o mock | Circuit breaker abre, saldo responde sem conversão *(só se o extra 2 existir)* |

O item 5 é o mais valioso: é o único que testa a garantia do banco em vez da sua. O item 4
é o que mais reprova o autor — é onde se descobre que existia timeout no HTTP mas não na
query.

**Entregável:** `docs/resilience-report.md` com a tabela acima mais uma coluna, "o que de
fato aconteceu". Hipóteses erradas ficam no texto, não são corrigidas retroativamente.

**Pronto quando:** os seis roteiros foram executados e o que os itens 3 e 4 quebraram
foi consertado.

---

## 7. O experimento

**A pergunta:** qual estratégia de concorrência sustenta mais transferências por segundo
mantendo a invariante de saldo não-negativo, e sob qual perfil de contenção?

**Cenários:**

| Cenário | Desenho | O que expõe |
|---|---|---|
| Alta contenção | Todas as transferências tocam a mesma conta | Serialização, espera em lock, taxa de retry |
| Baixa contenção | Transferências espalhadas em 100 contas | Overhead da estratégia quando não há disputa |

**Métricas a registrar:**

| Métrica | Ferramenta |
|---|---|
| Transferências/s sustentadas | harness próprio |
| p50 / p95 / p99 de latência | harness próprio |
| Taxa de retry por erro de serialização | contador na aplicação |
| Deadlocks detectados | log do Postgres |
| Alocações por operação no caminho quente | `go test -bench -benchmem` |
| Perfil de CPU sob carga | `pprof` |

Trate como resultado experimental: hardware, versão do Postgres, dataset, comandos e
número de repetições registrados no relatório. Reprodutibilidade é metade da
credibilidade — a outra metade é explicar **por que** o vencedor vence, não só que venceu.

---

## 8. ADRs a escrever

Arquivos curtos em `docs/adr/`: contexto, opções consideradas, decisão, consequências.

| # | Decisão | O que defender |
|---|---|---|
| 1 | Saldo derivado de ledger append-only em vez de coluna | Auditabilidade e impossibilidade de divergência silenciosa. **Custo assumido:** leitura vira `SUM` sobre N lançamentos |
| 2 | `int64` de centavos em vez de float ou decimal | Float perde centavo no arredondamento; decimal aloca no caminho quente |
| 3 | Idempotência escopada com fingerprint e resposta armazenada | Chave global colide entre donos; sem fingerprint, o mesmo identificador com corpo diferente passa silenciosamente |
| 4 | Estratégia de concorrência escolhida por medição | O ADR mais valioso. Duas implementações, dois cenários, números, e o preço de cada escolha |
| 5 | Cotação externa fora do caminho de escrita | A verdade financeira não pode depender da disponibilidade de terceiro, e valor convertido gravado no ledger nasce errado no minuto seguinte. Conversão é enriquecimento de leitura, com cache, timeout, circuit breaker e fallback |

---

## 9. Stack

```
Linguagem   Go 1.23+
HTTP        net/http + chi
Banco       PostgreSQL 16 · pgx/v5 · golang-migrate
Testes      testify · testcontainers-go · go test -race
Carga       harness próprio em Go
Perfil      go test -bench -benchmem · pprof
Infra       Docker Compose
CI          GitHub Actions · golangci-lint
```

Biblioteca padrão sempre que a diferença for pequena.

---

## 10. Estrutura do README final

1. Uma frase dizendo o que o sistema faz
2. Diagrama simples de arquitetura
3. O modelo do ledger, com o exemplo de uma transferência
4. Decisões de design, com link para cada ADR
5. Resultados de medição — tabela e metodologia
6. Relatório de falha induzida — hipótese, observado, divergência
7. Como rodar: `docker compose up` e os testes passando
8. **Limitações conhecidas**

O item 8 é o que mais impressiona. Nomear o que o sistema não resolve — sem mensageria,
sem multi-moeda, sem autenticação real, leitura de saldo degrada com o número de
lançamentos — é sinal de julgamento mais forte que qualquer feature a mais.

---

## 11. Fila de extras

Só depois da fase D, nesta ordem:

1. **Snapshot de saldo** — tabela de saldo consolidado até um `entry_id` de corte; saldo
   atual é snapshot + soma dos posteriores. Gera um segundo relatório antes/depois,
   com baseline honesto (índice coberto, não `SUM` sem índice)
2. **Conversão de saldo com cotação real** — único item do projeto que exercita
   resiliência contra dependência externa, que a vaga lista nominalmente. Detalhado abaixo
3. **Terceira estratégia de concorrência** — `pg_advisory_xact_lock` por conta
4. **Deploy em host único** com Terraform e Caddy para TLS
5. **Outbox transacional sem broker** — evento na mesma transação SQL, relay fazendo poll

### Detalhe do item 2

`GET /accounts/{id}/balance` devolve o saldo em BRL — a verdade do ledger — e, opcionalmente,
o equivalente em outra moeda, com a cotação usada e o horário dela.

**Regra inegociável:** o dado externo não entra no caminho de escrita. Nenhuma transferência
depende da API de cotação, nenhum valor convertido é persistido em `entries`. A conversão é
enriquecimento de leitura e nada mais.

O que o adapter externo precisa demonstrar:

| Mecanismo | Comportamento |
|---|---|
| Timeout | Curto e explícito, com `context` propagado |
| Cache | Cotação vale por N minutos; a maioria das requisições não sai da máquina |
| Circuit breaker | Após N falhas, para de tentar por um período |
| Fallback | Último valor conhecido, marcado como defasado na resposta |
| Degradação graciosa | API fora → endpoint responde mesmo assim, só sem a conversão |

Isso responde diretamente a duas perguntas de entrevista: o que o cliente vê quando uma
dependência cai, e por que a cotação não participa da transação de transferência.

**Não confundir com multi-moeda.** O ledger continua sendo só BRL: entries não ganham
moeda, saldos não somam entre moedas, não existe conta de câmbio. Multi-moeda de verdade
permanece na seção de limitações conhecidas.

---

## 12. Como começar

A primeira sessão produz, nesta ordem:

1. Repositório com a estrutura de pastas e o CI rodando lint + test
2. Migration 001 com as quatro tabelas
3. `Money` e `Transfer` no domínio, com seus testes — **sem banco nenhum ainda**
4. Só então o adapter Postgres e o primeiro teste de integração

Comece pelo domínio. Se começar pelo handler, a hexagonal vira decoração.