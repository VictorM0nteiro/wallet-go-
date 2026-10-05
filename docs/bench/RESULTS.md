# Resultados de medição

Registro de todos os testes de carga executados até aqui, com o que cada um mostrou e o
que ainda não se sabe. Os relatórios JSON de cada execução estão nesta pasta e podem ser
conferidos contra os números abaixo.

Os resultados são **comparações relativas entre configurações na mesma máquina**. Não
são capacidade de produção.

## Ambiente

| Item | Valor |
|---|---|
| Processador | 6 núcleos, 12 threads (hyperthreading) |
| Memória | ~15 GiB |
| Sistema | Windows 11, Docker Desktop com backend WSL2 |
| PostgreSQL | 16.15, container `postgres:16` |
| Go | 1.26.0 (toolchain local) |
| API e gerador | mesma máquina, sem isolamento de CPU |

## Ressalvas gerais

- **Ruído do host.** O gerador, a API, o Postgres e a VM do Docker dividem os mesmos
  12 threads. A linha de base de CPU variou de 4% a 69% entre rodadas idênticas, e a
  vazão acompanhou essa variação (ver a seção de pool).
- **Valor de `MaxConns` não está no JSON.** Os relatórios de 10 conexões e de 30 foram
  identificados pela ordem das execuções, não por um campo gravado. Isso é uma lacuna a
  fechar (ver "O que falta").
- **Versões diferentes da ferramenta.** As medições preliminares com 10 conexões foram
  feitas antes do escalonamento de workers, da seed e do relatório JSON. Por isso não
  têm arquivo nesta pasta.
- **Tamanho do banco cresce.** As transferências acumulam linhas. O banco foi de ~8 MB
  para ~268 MB ao longo destes testes. Isso pode afetar o cenário `read`.

## 1. Testes de validação da ferramenta

Esses testes não medem o sistema. Servem para checar que a ferramenta se comporta como
descrita.

| Arquivo | Teste | Resultado |
|---|---|---|
| `hot-seed9-…json` | Limite de CPU de 1% | Parou sozinho na primeira etapa (8 workers) com `resource_limit`. Confirma que o observador de host dispara. |
| `hot-seed10-…json` | `-rate 200`, 4 workers | Vazão de 193 req/s para alvo de 200. Confirma o ritmo por worker. |

## 2. Pool de conexões: 10 contra 30

Cenário `spread` (transferências entre 100 contas), nível fixo de 32 workers, seed 42,
etapa de 15 s. Todas com 0 erros de aplicação e 0 de conexão.

| Conexões | Relatório | Linha de base CPU | req/s | p99 | Parada |
|---|---|---|---|---|---|
| 10 (preliminar) | sem JSON | — | ~1216 | — | — |
| 10 (preliminar) | sem JSON | — | ~1293 | — | — |
| 30 | `…173521` | 27.6% | 1806 | 32 ms | max_workers |
| 30 | `…173537` | 4.2% | 1833 | 38 ms | host CPU 98% |
| 30 | `…173551` | 47.7% | 1642 | 42 ms | host CPU 99% |
| 30 | `…173657` | — | 2154 | 16 ms | host CPU 94% |
| 30 | `…173659` | — | 2753 | 18 ms | max_workers |
| 30 | `…173715` | — | 2456 | 24 ms | max_workers |
| 30 (limite CPU 100%) | `…174124` | 68.8% | 1765 | 36 ms | max_workers |
| 30 (limite CPU 100%) | `…174141` | 24.9% | 2482 | 22 ms | max_workers |
| 30 (limite CPU 100%) | `…174157` | 15.4% | 2722 | 18 ms | max_workers |

**Leitura:**

- Com 10 conexões, a pool limitou o `spread`: a vazão ficou perto de 40% do teto do
  banco medido com `pgbench` (ver seção 4).
- Com 30 conexões, a média das rodadas ficou em torno de 2100 req/s, mas a dispersão foi
  alta (1642 a 2753).
- Nas três últimas rodadas, com a linha de base de CPU caindo de 69% para 15%, a vazão
  subiu de 1765 para 2722 req/s e o p99 caiu de 36 para 18 ms. Esse é o padrão mais claro
  do conjunto, mas são três pontos, não uma prova.
- A melhor estimativa para 30 conexões em condições limpas é **2500 a 2700 req/s**. Os
  números de 10 conexões não foram feitos nas mesmas condições, então a diferença exata
  não está medida com rigor.

## 3. Cenário `hot` (todas as transferências na mesma conta)

| Conexões | Relatório | Etapas (workers: req/s, p99) | Parada |
|---|---|---|---|
| 10 | `…1791231999…` | 8: 514, 19 ms · 16: 385, 47 ms · 32: 326, 111 ms · 64: 319, 246 ms · 128: 309, 440 ms · 256: 285, 934 ms · 512: 258, 2030 ms | API (p99 > 2 s) em 512 |
| 30 | `…1791232403…` | 8: 508, 19 ms · 16: 391, 47 ms · 32: 324, 110 ms · 64: 284, 268 ms · 128: 260, 526 ms · 256: 318, 835 ms · 512: 271, 2330 ms | API (p99 > 2 s) em 512 |

**Leitura:** a vazão fica em torno de 300 req/s em qualquer tamanho de pool. O gargalo é
o lock da linha da conta, que serializa as transferências. Mais conexões não ajudam aqui,
e o p99 cresce linearmente com os workers, como previsto pela lei de Little.

## 4. Teto do banco com `pgbench`

Workload padrão (TPC-B), escala 10, 30 s por rodada, 4 threads de cliente, executado
dentro do container do Postgres num banco separado (`pgbench_test`).

| Clientes | tps médio | latência média |
|---|---|---|
| 8 | 2687 | 3.0 ms |
| 16 | 3380 | 4.7 ms |
| 32 | 2567 | 12.5 ms |

**Leitura:** nesta máquina, o banco sustenta cerca de 2500 a 3400 tps para esse workload.
A transação da API faz mais instruções (dois `SELECT ... FOR UPDATE`, um `SUM`, inserts em
transferência, entries e registro de idempotência), então o teto por requisição é menor.
Com 30 conexões, a API chega a 60-80% desse teto.

## 5. Medições preliminares (sem relatório JSON)

Feitas com a versão inicial da ferramenta, antes de escalonar workers. Os números são
da saída do terminal.

- **`hot`, 10 conexões, 512 workers:** 192 req/s, com 5,5% de `connection_refused`. Esse
  foi o primeiro indício de que a falha era de conexão e não de latência.
- **`spread`, 10 conexões, 32 workers:** 1293 e 1216 req/s em duas rodadas.
- **`read`, 10 conexões:** cerca de 6300 a 6900 req/s entre 8 e 1024 workers, com
  `connection_refused` acima de 512.
- **`hot` com `-max 5000`, 30 s por etapa:** 265 req/s em 64 workers, caindo para 191 em
  512, quando o p99 passou de 2 s.

## O que ainda falta

- Gravar `MaxConns` e a linha de base de CPU e RAM **no próprio JSON**, para não depender
  de inferência pela ordem das execuções.
- Repetir 10 e 30 conexões com a linha de base abaixo de 20%, cinco rodadas cada, e
  comparar medianas.
- Repetir o cenário `read` com o banco atual, já que ele cresceu bastante.
- Medir o `spread` com 20 e 40 conexões, para encontrar o pico da curva.
- Explicar por que as falhas de conexão apareceram no estágio de 512 workers nas medições
  preliminares. A hipótese é a fila de `accept` do Windows, ainda não confirmada.
