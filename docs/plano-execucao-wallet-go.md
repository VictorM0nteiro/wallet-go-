# wallet-go — Plano de execução

> Companheiro do documento de escopo. O escopo define **o que** e **por quê**; este define
> **quando** e **como**. 14 sessões de ~2h30.

---

## 1. Como trabalhar com o Claude Code neste projeto

O projeto existe para você conseguir defendê-lo falando, numa entrevista, sem consultar o
código. Isso muda o que faz sentido delegar.

### Delegue sem culpa

Boilerplate de adapter, DTOs, scaffolding de teste table-driven, configuração de CI e
lint, `docker-compose.yml`, Makefile, o harness de carga, scripts de seed, formatação de
relatório, rascunho de ADR a partir de decisão que **você** já tomou.

### Não delegue

- **O domínio** (`internal/domain/`). É a parte que a entrevista cobra, e código que você
  não escreveu você não defende sob pressão.
- **A decisão** de qualquer ADR. O Claude Code pode redigir; a escolha é sua.
- **A análise da medição.** "Por que a estratégia X venceu" é a pergunta de entrevista
  inteira. Se a resposta vier pronta, o projeto perdeu o propósito.

### A regra de aceitação

Não faça commit de código que você não consiga explicar em dois minutos, em voz alta, sem
olhar. Se não conseguir, peça explicação antes de aceitar — e se ainda assim não fechar,
reescreva à mão. Essa regra é o que separa este projeto de um tutorial copiado.

### `CLAUDE.md` na raiz

Crie na sessão 1. Ele é lido automaticamente e evita repetir contexto toda sessão:

```markdown
# wallet-go

API de carteira digital. Saldo é derivado de um ledger append-only; transferências são
idempotentes e corretas sob concorrência.

## Regras invioláveis
- `internal/domain/` não importa pgx, net/http, nem nada de infraestrutura.
- Dinheiro é `Money` (int64 de centavos). Nunca float, nem em log, nem em JSON.
- JSON expõe `amount_cents` inteiro. Formatação "R$ 0,00" só em apresentação.
- Ledger é append-only: nada de UPDATE ou DELETE em `entries`.
- Saldo nunca é coluna. É SUM sobre entries.
- Erros de domínio são valores tipados comparados com `errors.Is`.
- Locks são adquiridos em ordem crescente de `account_id`, sempre.
- Idempotência mora em `internal/app/`, não no handler HTTP.

## Comandos
make test        # go test -race ./...
make lint        # golangci-lint run
make migrate-up
make migrate-down
make db-up       # docker compose up -d

## Estado atual
Sessão N — <o que está pronto>
```

Atualize a linha de estado ao fim de cada sessão. É o que faz a sessão seguinte começar
sem preâmbulo.

---

## 2. Convenções

**Branches.** Uma por sessão: `s01-estrutura`, `s02-repositorios`. PR para `main` ao fim.
O objetivo não é cerimônia — é que o gatilho `pull_request` do CI seja exercitado, que é
como funciona em time.

**Commits.** Imperativo, uma mudança lógica por commit. Prefixo convencional (`feat:`,
`fix:`, `test:`, `docs:`, `chore:`) é opcional mas ajuda a gerar o histórico que você vai
citar.

**Testes.** Table-driven como padrão. Nome do caso descreve o comportamento, não a
implementação: `saldo_exatamente_igual_ao_valor_permite_debito`.

**Nada de `pkg/`.** Só existe o que é genuinamente reutilizável fora do projeto, e neste
projeto isso é nada.

---

## 3. Ritmo e critério de sessão

Cada sessão tem um **critério de pronto verificável**. Se o critério não fechar, a sessão
seguinte começa terminando a anterior — não acumule dívida para o fim.

Sessão que estourar duas vezes seguidas é sinal de escopo mal dimensionado, não de falta
de esforço. Nesse caso, corte da fila de extras antes de cortar de qualquer fase.

---

# FASE A — núcleo (sessões 1-3)

## Sessão 1 — estrutura, CI e domínio inicial

**Objetivo:** repositório que compila, CI verde, e a primeira regra de negócio testada sem
banco.

| Bloco | Tempo | Tarefa |
|---|---|---|
| 1 | 20min | Pastas, `main.go` com `signal.NotifyContext`, `CLAUDE.md` |
| 2 | 25min | `.golangci.yml` (v2) e `.github/workflows/ci.yml` |
| 3 | 25min | `docker-compose.yml`, migration 001, Makefile |
| 4 | 50min | `Money`, erros do domínio, `CanDebit`, testes |

**Casos de teste obrigatórios do bloco 4:**
- soma e subtração
- `CanDebit` com saldo exatamente igual ao valor → permite (invariante é ≥ 0)
- `CanDebit` com saldo um centavo menor → `ErrInsufficientFunds`
- `CanDebit` na conta `system` → decisão consciente, documentada no teste
- construtor com zero e com negativo
- `String()` para 0, 1, 99, 100, 9700 — o caso `1` é `"R$ 0,01"`
- overflow em `math.MaxInt64` — escolha entre panic, erro ou limitação documentada

**Pronto quando:** `go test ./internal/domain/...` passa em menos de 1s sem Docker aberto,
e o CI está verde.

**Armadilhas:** `go test -race` no Windows exige mingw-w64 instalado. Ciclo
`migrate up → down → up` precisa funcionar; se o `down` não existir, você vai destruir o
volume toda vez que errar o schema.

---

## Sessão 2 — persistência

**Objetivo:** falar com o Postgres a partir de teste automatizado.

- Pool pgx configurado com `MaxConns`, `MaxConnLifetime` e **timeout de aquisição**
- `internal/adapters/postgres/`: repositório de contas (criar, buscar por id) e de entries
  (inserir lote, somar saldo por conta)
- Helper de teste com `testcontainers-go` que sobe Postgres, roda as migrations e devolve
  um pool limpo por teste
- Primeiro teste de integração: cria conta, insere um par de entries, lê o saldo

**Pronto quando:** o teste de integração passa do zero, sem banco pré-existente, só com
Docker rodando.

**Armadilhas:** o timeout de aquisição de conexão parece detalhe e é o que a fase E vai
testar no roteiro 4 — sem ele, banco lento vira requisição pendurada em vez de erro
rápido. Não compartilhe container entre testes até isso doer; isolamento primeiro,
velocidade depois.

---

## Sessão 3 — a transferência

**Objetivo:** a operação central funcionando ponta a ponta, sem HTTP.

- A porta no `app/`: uma interface de um método que executa a transferência atomicamente
- Caso de uso `Transferir`: valida entrada, chama a porta, traduz erro
- Implementação 1 no adapter Postgres, usando `SELECT ... FOR UPDATE` **em ordem crescente
  de `account_id`**, uma transação: trava, lê saldos, chama `domain.CanDebit`, insere
  `transfers` + duas `entries`, commita
- Testes de integração: transferência feliz, saldo insuficiente, conta inexistente,
  mesma conta origem e destino
- As duas invariantes da seção 3 do escopo como teste SQL executado no CI

**Pronto quando:** as duas invariantes rodam no CI, e a transferência com saldo
insuficiente não deixa nenhuma entry no banco.

**Armadilhas:** a tentação de colocar a leitura de saldo dentro do domínio. O domínio
recebe o saldo como parâmetro — quem busca é o adapter, dentro da transação. Se inverter,
a fase C não consegue trocar de estratégia sem tocar em regra de negócio.

---

# FASE B — apresentável (sessões 4-6)

## Sessão 4 — idempotência

**Objetivo:** dois cliques no botão não transferem duas vezes.

- Fluxo no `app/`, não no handler: recebe chave e fingerprint como parâmetros
- `INSERT ... ON CONFLICT DO NOTHING` na tabela de chaves, dentro da mesma transação da
  transferência
- Fingerprint = sha256 do corpo canonicalizado (campos ordenados, sem espaço irrelevante)
- Os quatro casos da tabela do escopo: chave nova, replay idêntico, replay com corpo
  diferente (422), chave `in_flight` (409)
- **Teste de concorrência:** N goroutines disparam a mesma chave simultaneamente; asserção
  de exatamente um `transfer` e exatamente duas `entries`

**Pronto quando:** o teste de concorrência passa sob `-race`, repetido 20 vezes seguidas
sem flake.

**Armadilhas:** guardar só o id do transfer não basta — o replay precisa devolver o corpo
e o status originais. E a chave escopada por `(scope, endpoint, key)`, nunca global.

---

## Sessão 5 — HTTP

**Objetivo:** os seis endpoints, com tradução de erro correta.

- chi com os seis endpoints do escopo
- DTOs de entrada e saída separados das entidades de domínio
- Função de mapeamento erro de domínio → status HTTP, **testada isoladamente**
- Middleware: correlation ID por requisição, `log/slog` estruturado, recuperação de panic
- `context` propagado do handler até a query, com timeout por requisição
- Extrato paginado por cursor (`?after=<entry_id>&limit=`), nunca por offset

**Pronto quando:** `curl` faz o ciclo completo — cria duas contas, deposita, transfere,
consulta saldo e extrato.

**Armadilhas:** JSON com `amount_cents` inteiro. Se implementar `MarshalJSON` devolvendo
`"R$ 97,00"`, a ambiguidade de float volta pela borda. Offset em ledger append-only produz
página instável — use cursor.

---

## Sessão 6 — operável

**Objetivo:** o ponto de corte. Daqui em diante o projeto é apresentável mesmo que pare.

- `GET /healthz` (processo vivo) e `GET /readyz` (banco alcançável) — **separados**
- Graceful shutdown completo: `srv.Shutdown` com contexto de timeout, depois `pool.Close()`,
  nessa ordem
- Configuração por variável de ambiente, com validação na subida e falha rápida
- `docker-compose.yml` subindo app + banco + migrations
- README v1: a frase, como rodar, o modelo do ledger com o exemplo

**Pronto quando:** outra pessoa clona, roda `docker compose up`, e faz uma transferência
sem te perguntar nada.

**Armadilhas:** `/healthz` que checa o banco derruba o container inteiro quando o banco
pisca. Liveness não depende de dependência externa — readiness sim. Essa distinção é o
roteiro 1 da fase E.

---

# FASE C — medição (sessões 7-9)

## Sessão 7 — harness

**Objetivo:** capacidade de gerar carga e medir, antes de ter o que comparar.

- Gerador em Go: N workers, duração configurável, taxa alvo
- Dois cenários: **alta contenção** (todas as transferências tocam a mesma conta) e
  **baixa contenção** (espalhadas em 100 contas)
- Coleta: throughput, p50/p95/p99, contagem por tipo de erro, retries, deadlocks
- Seed determinístico, com semente registrada
- Saída em CSV ou JSON, um arquivo por execução, versionado em `docs/bench/`

**Pronto quando:** você roda o harness contra a estratégia 1 e obtém números estáveis
entre execuções repetidas.

**Armadilhas:** medir com o próprio harness competindo por CPU com o serviço na mesma
máquina. Registre isso como limitação; se tiver WSL2 ou uma VM, prefira medir lá.

### Pendências herdadas do loadtest preliminar

Um primeiro gerador já existe em `cmd/loadtest` (uso em `docs/loadtest.md`), criado
na Sessão 5 para forçar a API até quebrar. Rodado em 2026-09 na máquina de
desenvolvimento (Windows, Docker Desktop, gerador e API no mesmo host), ele deixou
estas pendências, que devem ser resolvidas aqui e não antes:

**Corrigir a ferramenta**

- [ ] **Separar o tipo de falha.** Hoje todas as "quebras" foram `connection_refused`
  (camada TCP, antes de a requisição chegar à aplicação); em nenhuma das 5 execuções
  houve um único `5xx`. No `read`, ele "quebrou" em 1024 workers com p99 de 217 ms.
  Relatar falha de conexão, falha de status e limite de latência como causas
  distintas, para o veredito não misturar limite do cliente ou do SO com saturação da API.
- [ ] **Escalonar o início dos workers** (ou pré-aquecer as conexões) dentro de cada
  estágio. Hipótese, ainda não verificada: ~250 conexões novas de uma vez estouram a
  fila de `accept` do Windows (~200). As recusas apareceram em todas as execuções
  exatamente no estágio de 512 workers (134 a 232). Se sumirem ao escalonar, era artefato.
- [ ] Taxa alvo, seed registrada e saída em arquivo (CSV ou JSON), como pedem os
  itens acima. O preliminar só imprime no terminal e usa seed baseada no relógio.

**Explicar o que ainda não se sabe** (a análise é sua, o Claude Code não escreve a conclusão)

- [ ] **Por que o `hot` piora com mais workers?** Observado: 302 req/s com 8 workers,
  162 a 190 com 128 a 512, apesar de o pool limitar o Postgres a 10 transações
  simultâneas. Escrever a hipótese antes. Experimento: `-start 10 -max 10` contra
  `-start 128 -max 128`, olhando `pg_stat_activity` (wait events) durante a execução.
- [ ] **Por que `spread` (~1.200 req/s) é ~5x mais lento que `read` (~6.500 req/s)?**
  Suspeita: custo de commit (fsync do WAL, lento no Docker Desktop). Prova possível:
  uma execução com `synchronous_commit=off`, usada só como experimento e registrada
  como tal, nunca como configuração final.
- [ ] **O `503` do `AcquireTimeout` (3 s) nunca foi observado.** No `hot` o p99 parou
  em 2,95 s, colado no limite, e a ferramenta parou antes por causa das recusas de
  conexão. Provocar de propósito (mais workers, com a correção acima) e confirmar que
  o comportamento é falhar rápido com `503`, não pendurar.

**Metodologia**

- [ ] **Ruído acima de 15%.** Mesma configuração, execuções diferentes: `hot` com 128
  workers deu 162 e 252 req/s; `spread` com 8 workers, 1.065 e 1.292. Pelo plano,
  variação acima de 15% entre repetições significa mudar o ambiente de medição (WSL2
  ou VM) antes de aumentar as repetições. Medir com 3 repetições por configuração.
- [ ] **Registrar o tamanho do banco** em cada execução (o volume chegou a ~900 MB
  depois dos testes preliminares). O `SUM` do saldo varre as entries da conta, então
  banco cheio pode mudar os números do cenário de leitura.
- [ ] Resultado preliminar que **não deve ser citado como conclusão**: "a API quebra
  em 512 workers". O que os dados preliminares sustentam: sob contenção alta a API
  degrada por fila (p50 ≈ workers / rps, sem erros de aplicação), e o ledger ficou
  consistente em todas as execuções.

---

## Sessão 8 — segunda estratégia

**Objetivo:** ter o que comparar.

- Implementação 2 da porta: transação em `SERIALIZABLE` com retry no erro de serialização
- Detecção correta do código de erro do Postgres (`40001`) e do de deadlock (`40P01`)
- Backoff com jitter entre tentativas, limite de tentativas, contador exportado
- Seleção da estratégia por variável de ambiente
- Os testes de integração da sessão 3 rodam contra **as duas** implementações

**Pronto quando:** a mesma suíte passa nas duas estratégias sem alteração de teste.

**Armadilhas:** em `SERIALIZABLE` o retry precisa refazer a leitura, não reaproveitar
valor lido na tentativa anterior. Retry cego em cima de estado velho reintroduz o bug que
o isolamento existia para impedir.

---

## Sessão 9 — execução

**Objetivo:** dados brutos, reprodutíveis.

- Matriz completa: 2 estratégias × 2 cenários × 3 repetições
- pprof de CPU e heap na estratégia vencedora sob alta contenção
- `go test -bench -benchmem` no caminho quente do domínio
- Registrar por execução: hardware, SO, versão do Postgres, versão do Go, parâmetros,
  semente, comando exato
- Dados brutos commitados sem edição

**Pronto quando:** `docs/bench/` tem os arquivos e qualquer pessoa consegue repetir a
execução a partir do que está escrito.

---

# FASE D — documentação (sessões 10-12)

## Sessão 10 — relatório de performance

- Tabela comparativa: estratégia × cenário × métrica
- Gráfico simples de throughput e p99 por cenário
- **A explicação**, que é o entregável real: por que a vencedora vence, sob qual carga a
  ordem se inverte (ou por que não se inverte), e qual foi o preço da escolha
- Ameaças à validade declaradas: ambiente, ruído, escala do dataset

**Armadilha:** apresentar número absoluto como throughput de produção. Você mediu
comparação relativa em ambiente controlado — diga exatamente isso.

## Sessão 11 — ADRs

Os cinco do escopo, em `docs/adr/`, formato curto: contexto, opções consideradas, decisão,
consequências. Cada um em menos de uma página.

O ADR 4 (estratégia de concorrência) cita o relatório. O ADR 1 (saldo derivado) declara o
custo e aponta o snapshot como mitigação futura.

## Sessão 12 — README final

Os oito itens da seção 10 do escopo. Diagrama simples de arquitetura. Seção de limitações
conhecidas escrita com cuidado — é o item que mais pesa.

**Pronto quando:** alguém que nunca viu o projeto entende, em cinco minutos de leitura, o
que ele faz, por que as decisões foram tomadas e o que ele não resolve.

---

# FASE E — falha induzida (sessões 13-14)

## Sessão 13 — roteiros 1 a 3

Postgres cai, Postgres volta, SIGTERM sob carga. Hipótese escrita **antes** de cada um.

Reserve metade da sessão para consertar o que o roteiro 3 quebrar. Ele quase sempre quebra.

## Sessão 14 — roteiros 4 a 6 e relatório

Latência artificial, `kill -9` no meio de uma transferência, dependência externa fora (se
o extra 2 existir).

`docs/resilience-report.md` com a coluna "o que de fato aconteceu". **Hipóteses erradas
ficam no texto.** A divergência é o produto.

---

## 4. Riscos e contingência

| Risco | Sinal | Resposta |
|---|---|---|
| Fase A estoura | Sessão 3 não fecha na semana 3 | Corte o extrato paginado da sessão 5; ele não é central |
| Testcontainers lento demais no Windows | Suíte acima de 2min | Container compartilhado por pacote, com truncate entre testes |
| Medição ruidosa | Variação acima de 15% entre repetições | Mude o ambiente de medição antes de aumentar repetições |
| Processo seletivo abre antes da fase C | — | Pare na sessão 6, escreva o README v1 declarando o estado, apresente assim |
| Motivação cai | Duas sessões puladas | Volte pelo bloco mais curto disponível, não pelo mais importante |

---

## 5. Checklist final

- [ ] `go test -race ./...` verde no CI
- [ ] As duas invariantes testadas automaticamente
- [ ] Teste de concorrência de idempotência sem flake em 20 execuções
- [ ] `docker compose up` funciona em máquina limpa
- [ ] `/healthz` e `/readyz` separados e corretos
- [ ] Graceful shutdown drena requisições em voo
- [ ] Relatório de performance com metodologia e ameaças à validade
- [ ] 5 ADRs escritos
- [ ] Relatório de resiliência com pelo menos uma hipótese errada preservada
- [ ] README com limitações conhecidas
- [ ] Você consegue responder, em voz alta e sem olhar, as 8 perguntas de defesa do projeto

O último item é o único que não tem código associado e é o que decide a entrevista.
