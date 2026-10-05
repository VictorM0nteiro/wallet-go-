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
  12 threads. A linha de base de CPU variou de 1% a 69% entre rodadas idênticas.
- **O tamanho da pool é a variável mais fácil de errar.** A API só usa o valor novo
  depois de ser **reconstruída** (`docker compose up -d --build app`). Por um tempo, as
  rodadas rotuladas como "30" rodaram com 10 conexões, porque o container não tinha sido
  reconstruído. Ver a seção 2.
- **O JSON só registra o tamanho da pool se o operador informar** (`-api-max-conns`). Não
  é lido da API. Quando o rótulo informado não bate com a realidade, o relatório fica
  errado sem nenhum aviso.
- **Versões diferentes da ferramenta.** As medições preliminares foram feitas antes do
  escalonamento de workers, da seed e do relatório JSON. Por isso não têm arquivo nesta
  pasta.
- **O banco cresce.** As transferências acumulam linhas. O banco foi de ~8 MB para
  ~268 MB ao longo destes testes, o que pode afetar o cenário `read`.

## 1. Testes de validação da ferramenta

Esses testes não medem o sistema. Servem para checar que a ferramenta se comporta como
descrita.

| Arquivo | Teste | Resultado |
|---|---|---|
| `hot-seed9-…json` | Limite de CPU de 1% | Parou sozinho na primeira etapa (8 workers) com `resource_limit`. Confirma que o observador de host dispara. |
| `hot-seed10-…json` | `-rate 200`, 4 workers | Vazão de 193 req/s para alvo de 200. Confirma o ritmo por worker. |

## 2. Pool de conexões: curva de 10 a 50

Cenário `spread` (transferências entre 100 contas), nível fixo de 32 workers, seed 42,
etapa de 15 s. Nenhuma rodada teve erro de aplicação ou de conexão.

As conexões reais de cada bloco vêm da **ordem das execuções e da reconstrução do
container**, não do JSON. Essa é a classificação correta:

| Bloco | Arquivos | Conexões reais | Label no JSON | n | Mediana req/s | Média req/s | Linha de base CPU (média) |
|---|---|---|---|---|---|---|---|
| A | `…173521` a `…174157` | 10 | sem campo | 9 | 2154 | 2179 | não registrada |
| B | `…174932` a `…175037` | 10 | **30 (errado)** | 5 | 2477 | 2357 | ~6% |
| C | `…175150` a `…175256` | **30** | 30 | 5 | **3667** | 3569 | ~11% |
| D | `…175424` a `…175529` | 10 | **30 (errado)** | 5 | 2529 | 2380 | ~6% |
| E | `…180048` a `…180153` | **20** | 20 | 5 | 3312 | 3341 | ~7% |
| F | `…180246` a `…180352` | **40** | 40 | 5 | 3321 | 3458 | ~7% |
| G | `…180447` a `…180553` | **50** | 50 | 5 | 3324 | 3432 | ~6% |

Os blocos B e D tiveram o campo `api_max_conns_declared` preenchido com 30 porque o
argumento na linha de comando não foi atualizado quando o `MaxConns` voltou para 10. Os
relatórios estão corretos em todos os outros campos. Os JSON não foram editados, porque
são dados de medição. Os blocos B e D devem ser lidos como 10 conexões.

**Curva (mediana, 5 rodadas por bloco):**

| Conexões | Mediana req/s |
|---|---|
| 10 | ~2500 |
| 20 | 3312 |
| 30 | 3667 |
| 40 | 3321 |
| 50 | 3324 |

**Leitura:**

- **De 10 para 20 conexões há o ganho que se sustenta**: de ~2500 para ~3300 req/s (+33%).
  A diferença é grande e aparece em todos os blocos.
- **De 20 a 50 há um platô**, em torno de 3300 a 3700 req/s. O pico de 30 (3667) fica cerca
  de 10% acima do platô, mas essa diferença é menor que a dispersão dentro de cada bloco
  (16 a 30%). Com os dados atuais, não dá para afirmar que 30 é melhor que 20, 40 ou 50.
- **A linha de base não explica o platô.** Os blocos de 20, 40 e 50 tiveram CPU de fundo
  parecida (5 a 7%), e a vazão ficou no mesmo patamar.
- **Escolha operacional.** O `MaxConns` do código fica em **30**, que teve a maior mediana
  da curva. A escolha não é uma prova de otimalidade: qualquer valor entre 20 e 50 está no
  mesmo patamar, dentro do que as medições conseguem separar.

**Correção de textos anteriores.** A mensagem do commit `a7bb22a` diz que o `spread` foi
de ~1250 req/s com 10 conexões para ~2100-2700 com 30. Esses números estão errados. O
valor de ~1250 veio de uma versão anterior da ferramenta, e o de ~2100-2700 foram
rodadas com 10 conexões de fato. A comparação correta é a da tabela acima.

## 3. Cenário `hot` (todas as transferências na mesma conta)

| Conexões | Relatório | Etapas (workers: req/s, p99) | Parada |
|---|---|---|---|
| 10 | `…1791231999…` | 8: 514, 19 ms · 16: 385, 47 ms · 32: 326, 111 ms · 64: 319, 246 ms · 128: 309, 440 ms · 256: 285, 934 ms · 512: 258, 2030 ms | API (p99 > 2 s) em 512 |
| 10 (rótulo do JSON: 30) | `…1791232403…` | 8: 508, 19 ms · 16: 391, 47 ms · 32: 324, 110 ms · 64: 284, 268 ms · 128: 260, 526 ms · 256: 318, 835 ms · 512: 271, 2330 ms | API (p99 > 2 s) em 512 |

**Leitura:** a vazão fica em torno de 300 req/s com qualquer tamanho de pool. O gargalo é
o lock da linha da conta, que serializa as transferências. Mais conexões não ajudam aqui,
e o p99 cresce linearmente com os workers, como previsto pela lei de Little.

Os dois relatórios de `hot` foram feitos com 10 conexões de fato. O segundo teve o campo
`api_max_conns_declared` gravado como 30, mas o container ainda não tinha sido
reconstruído. Por ser um cenário limitado por lock, a conclusão não muda, mas o rótulo
precisa ser lido como 10.

## 4. Teto do banco com `pgbench`

Workload padrão (TPC-B), escala 10, 30 s por rodada, 4 threads de cliente, executado
dentro do container do Postgres num banco separado (`pgbench_test`).

| Clientes | tps médio | latência média |
|---|---|---|
| 8 | 2687 | 3.0 ms |
| 16 | 3380 | 4.7 ms |
| 32 | 2567 | 12.5 ms |

**Leitura e ressalva:** o `pgbench` mediu cerca de 2500 a 3400 tps nesta máquina, em
outro momento e com outra ferramenta. A API chegou a ~3300 a 3700 req/s com 20 a 50
conexões, então **esse número não é um teto confiável**, e não dá para afirmar que a API
está no limite do banco. Para usar o `pgbench` como referência, ele precisa ser repetido
nas mesmas condições (host ocioso, mesma janela de tempo) antes de qualquer comparação.

## 5. Medições preliminares (sem relatório JSON)

Feitas com a versão inicial da ferramenta, antes de escalonar workers. Os números são
da saída do terminal.

- **`hot`, 10 conexões, 512 workers:** 192 req/s, com 5,5% de `connection_refused`. Esse
  foi o primeiro indício de que a falha era de conexão e não de latência.
- **`spread`, 10 conexões, 32 workers:** 1293 e 1216 req/s em duas rodadas. Estes são
  números de uma versão antiga da ferramenta e não devem ser comparados diretamente com
  as seções 2 e 4.
- **`read`, 10 conexões:** cerca de 6300 a 6900 req/s entre 8 e 1024 workers, com
  `connection_refused` acima de 512.
- **`hot` com `-max 5000`, 30 s por etapa:** 265 req/s em 64 workers, caindo para 191 em
  512, quando o p99 passou de 2 s.

## O que ainda falta

- **Mais repetições dos blocos de 20 a 50 conexões**, para separar os valores dentro do
  platô, que hoje não se distinguem pela dispersão (16 a 30% dentro de cada bloco).
- **Confirmar o número real de conexões no banco durante uma rodada**, com
  `pg_stat_activity`. Hoje o valor vem da declaração do operador, não de uma medição.
- **Repetir o `pgbench`** nas mesmas condições da API, para ter uma referência válida.
- **Repetir o cenário `read`** com o banco atual, já que ele cresceu bastante.
- **Explicar as falhas de conexão** que apareceram no estágio de 512 workers nas medições
  preliminares. A hipótese é a fila de `accept` do Windows, ainda não confirmada.
- **Verificar o `503` do `AcquireTimeout` (3 s)**, que ainda não foi observado. Só dá para
  provocar com mais workers do que a pool suporta, e o limite de recurso precisa estar
  desligado para isso.
- **Corrigir o rótulo em rodadas futuras**: passar `-api-max-conns` sempre igual ao valor
  real da API, ou não passar nada quando não houver certeza.
