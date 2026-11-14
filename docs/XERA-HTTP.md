# Xera HTTP: deployment and observable behavior

[Русский](#эксплуатация) · [English](#deployment)

## Эксплуатация

Xera задаётся как `network: "xera-http"` с `xeraHttpSettings`. Клиенту и серверу нужны совместимые ядра. UUID проверяется VLESS после допуска HTTP-потока. Сам по себе правильный путь, session ID или tokenish-padding не подтверждает личность клиента: посторонний клиент может сформировать их самостоятельно.

Для защиты от запросов, не знающих настроек профиля, используйте REALITY либо согласованный `customDownlinkPadding` с непредсказуемым token на обоих концах. Дополнительный padding меняет формат данных; включение только на сервере прервёт соединения прежних клиентов. Пример содержимого `xeraHttpSettings`:

```json
{
  "path": "/configured-path/",
  "mode": "packet-up",
  "customDownlinkPadding": {
    "header": "X-Transfer-Context",
    "token": "REPLACE_WITH_RANDOM_BASE64URL_TOKEN",
    "bytes": "16-128",
    "blockBytes": "4096-8192",
    "budgetPercent": 5,
    "burstBytes": 4096,
    "uplink": true
  }
}
```

Замените token значением из 32 случайных байт в base64url, например через `openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'`. Это общий секрет профиля, доступный его клиентам и CDN, завершающему TLS. Не помещайте его в журналы или публичные примеры. Ровно один правильный маркер требуется до чтения тела и выделения сессии; пропущенный, неверный и дублированный маркеры получают обычный 404. Browser Dialer с этим расширением не поддерживается. Маркер статический: он не даёт защиты от повтора после его раскрытия и не заменяет UUID, сертификат или проверку REALITY.

Пределы `serverMaxSessions`, `serverMaxBufferedBytes` и `serverMaxHeaderBytes` ограничивают разные ресурсы. После отказа новая сессия освобождается сразу, если её не использует параллельный запрос и не был принят корректный upload. Успешные packet-up сессии ожидают downlink до 30 секунд; чтение одного packet-body ограничено отдельно. Ошибочный запрос не удаляет уже принятую сессию. Предел заголовков применяется также к HTTP/3; точный учёт служебных полей зависит от HTTP-версии.

Если h2Params.MaxReadFrameSize не задан или равен 0, наши HTTP/2-серверы Xera и XHTTP объявляют размер кадра 16384 байта (16 КиБ). Явное значение, например 65536, сохраняется. Это помогает уменьшить выбираемый клиентом буфер длительной загрузки при прямом подключении к нашему серверу, но не задаёт общий предел памяти. Внешний CDN объявляет свой размер кадра; этот default его не меняет.

В Xera HTTP/3, если streamSettings.finalmask.quicParams.maxIncomingStreams не задан или равен 0, лимит двунаправленных QUIC-потоков на соединение равен 2 × эффективный serverMaxSessions + 256. Параметр serverMaxSessions находится в xeraHttpSettings; если он не задан или равен 0, лимит сессий равен 4096, а лимит потоков — 8448. stream-up держит два длительных запроса на сессию, stream-one — один, а packet-up — downlink и конечные upload-запросы. Явные maxIncomingStreams, включая 3 и -1, сохраняются: -1 запрещает входящие двунаправленные потоки. При ручном лимите оставляйте место для запросов выбранного режима; QUIC-потоки и сессии считаются отдельно.

uTLS согласует ClientHello, но не превращает весь клиент в браузер. Частичные HTTP/2-профили Chrome 133 и 155 применяются только с соответствующим TLS-профилем; остальные профили используют стандартный HTTP/2-клиент. Для Chrome 155 сравнивались SETTINGS, окно соединения и порядок псевдозаголовков с официальным Chrome for Testing 155.0.8059.84, revision 1697595. Это не сравнение всего сетевого поведения браузера. Параметры HTTP/2 и HTTP-заголовки видны TLS-терминатору, а не автоматически любому промежуточному провайдеру. Длины TLS-записей, объём, направление и интервалы передачи остаются наблюдаемыми. Padding не гарантирует неотличимость от просмотра сайтов. Проверка против конкретного классификатора требует сравнительных захватов и измерения ложных срабатываний в целевой сети.

В Core 1.1.21 проверенные H2-профили Chrome 133/155 не используют начальный рост TLS-записей Go/uTLS. Очередь объединяет готовые данные без ожидания заполнения: до 64 КиБ pending и 64 КиБ active на соединение. Паузы packet-up по умолчанию выбираются из 20–40 мс; явный scMinPostsIntervalMs сохраняется. Новых фоновых запросов для имитации веб-сёрфинга нет. Это не обещание совпадения всех TLS-записей или TCP-сегментов с Chrome.

Явные minVersion/maxVersion проверяются и соблюдаются также при uTLS. Отпечаток без подходящей версии или несовместимый сервер отклоняется. Пользовательские ограничения версий могут изменить ClientHello. ECH и HTTP/3 требуют TLS 1.3.

У REALITY сохранён собственный fallback/spider: обычный доверенный сертификат вместо подтверждения REALITY может привести к HTTPS-запросам к настроенному серверу. Это не подтверждает доступ к VPN. Утверждение об отсутствии новых фоновых запросов относится к самому Xera.

### Выбор режима и пределы обхода

Состояние исследований проверялось на 14 ноября 2026 года. Работы по Китаю и РФ показывают разные механизмы: фильтрацию IP/SNI, анализ QUIC Initial, активные проверки и ограничение передачи после установления соединения. Дата публикации исследования не означает, что его метод продолжает действовать у каждого оператора. Отдельного подтверждённого описания упомянутого «ИИ-детектора» с набором данных и критериями обнаружения нет; Xera не имеет доказанной защиты от неизвестного классификатора.

| Условия | Настройка и проверка |
| --- | --- |
| Прямое соединение по TCP | REALITY с корректными ключами и `stream-auto`. REALITY проверяет свой допуск до HTTP; VLESS затем проверяет пользователя. Проверьте целевой сервер и фактическую доступность адреса. |
| CDN или прокси с буферизацией upload | TLS и `packet-up` либо `stream-auto`, который здесь выбирает конечные POST. Пригодность CDN проверяется передачей, а не успешным TLS. Обычное завершение TLS на CDN несовместимо с неизменённым REALITY. |
| UDP доступен и HTTP/3 нужен | Отдельный TLS-профиль с `alpn: ["h3"]`. Проверьте MTU, продолжительную передачу и восстановление после простоя. QUIC Initial сам по себе не скрывает SNI; нужен реально согласованный ECH, если его поддерживает сервер. |
| UDP блокируется | Используйте отдельный профиль HTTP/2 по TCP с доступным адресом. В Xera нет автоматического переноса открытого H3-потока в H2. Смена режима на том же заблокированном IP не создаёт другой маршрут. |
| Передача обрывается после небольшого объёма | Проверяйте длительную загрузку и отдачу, включая вложенный HTTPS. Размер HTTP-запроса не равен суммарному объёму TCP-соединения. Дробление POST не гарантирует обход ограничения на всё соединение. |
| Строгий список разрешённых адресов | Сначала требуется разрешённый и доступный маршрут. Padding и ClientHello не восстанавливают пакеты, отбрасываемые до TLS. |

CDN, завершающий TLS, видит HTTP и содержимое туннеля после снятия внешнего TLS и padding. При VLESS без отдельного шифрования это включает служебные данные VLESS; внутренний HTTPS сохраняет собственную защиту. Не считайте padding шифрованием или защитой от самого CDN.

Для IPv6 задавайте литерал в поле `address`, например `2001:db8::1`; в URL и паре адрес:порт нужны скобки: `[2001:db8::1]:443`. Пример использует зарезервированный адрес документации. Для SNI используйте имя, соответствующее настройкам TLS/REALITY. Поддержка IPv6 в Core не создаёт маршрут IPv6 на хосте или в Docker и не снимает запрет ОС. Проверяйте IPv4 и IPv6 отдельно, включая отдельный адрес `downloadSettings`; DNS, семейство адресов и страна выхода — разные свойства. Автовыбор клиента должен проверять доступность через выбранный адрес, а не полагаться на проверку от панели.

### Browser Dialer и ECH

Browser Dialer использует настоящий браузер и требует отдельной открытой страницы помощника. Держите её на loopback и открывайте напрямую по настроенному адресу. Страница и WebSocket проверяют Host/Origin, не разрешают произвольным сайтам читать токен, не кэшируют страницу и не допускают её в iframe. После обновления Core перезагрузите уже открытую страницу помощника. Это защита от посторонних веб-страниц, а не от процесса, уже имеющего локальный доступ.

Fetch не следует редиректам, не использует кэш туннеля и не передаёт тело ошибочного HTTP-ответа как данные соединения. Для Browser Dialer используйте размещение session/sequence/padding в path, query или заголовках, а данных — в body либо заголовках. Установить cookie другого origin из страницы помощника нельзя: cookie-размещения и явный Cookie отклоняются до подключения. GET/HEAD с body и запрещённые Fetch методы также отклоняются. Для обычного native-клиента эти cookie-размещения сохраняются; `customDownlinkPadding` остаётся несовместимым с Browser Dialer.

ECH скрывает внутренний ClientHello только при фактическом согласовании. Остаются видимыми IP, внешний SNI и характеристики обмена. Получение ECH-конфигурации — отдельная зависимость: `udp://` раскрывает DNS-запрос, а доступность выбранного DoH нужно проверить в целевой сети. Крупные HTTPS/ECH-записи поддерживаются; усечённый UDP-ответ повторяется по TCP у того же резолвера с теми же socket settings. Ответ DoH ограничен 65535 байтами и проверяется перед кэшированием; при ошибке DoH нет перехода на незашифрованный DNS. Ошибка ECH не разрешает незаметно отключить ECH или проверку сертификата.

## Deployment

Configure `network: "xera-http"` and `xeraHttpSettings` on compatible client and server cores. VLESS authenticates the UUID after HTTP stream admission. A valid path, session ID or tokenish padding is not a credential; another client can construct these values.

Use REALITY or matching `customDownlinkPadding` with an unpredictable profile token to reject requests that do not know the profile settings. The JSON above belongs inside `xeraHttpSettings`. Generate the token from 32 random bytes, for example with the OpenSSL command above; never use the placeholder. Additional padding changes the data framing, so enabling it only on the server breaks older clients.

The token is shared with profile clients and visible to a TLS-terminating CDN. Keep it out of logs and public examples. Missing, incorrect or duplicate markers return the same ordinary 404 before body reads or session allocation. Browser Dialer is unsupported with this extension. The static marker does not prevent replay after disclosure and does not replace VLESS credentials, certificate verification or REALITY authentication.

`serverMaxSessions`, `serverMaxBufferedBytes` and `serverMaxHeaderBytes` bound different resources. Failed admission immediately releases a new session unless another request is using it or a valid upload has already been accepted. Successful packet-up sessions retain their 30-second downlink wait; individual packet-body reads have a separate deadline. An invalid request does not delete an accepted session. HTTP/3 also enforces the configured header limit; accounting for protocol metadata differs between HTTP versions.

If h2Params.MaxReadFrameSize is unset or 0, our Xera and XHTTP HTTP/2 servers advertise a frame size of 16384 bytes (16 KiB). An explicit value, such as 65536, is preserved. This helps reduce the client-selected long-upload buffer when connecting directly to our server, but does not set a total memory limit. An external CDN advertises its own frame size; this default does not change it.

For Xera HTTP/3, if streamSettings.finalmask.quicParams.maxIncomingStreams is unset or 0, the bidirectional QUIC stream limit per connection is 2 × effective serverMaxSessions + 256. serverMaxSessions belongs in xeraHttpSettings; when unset or 0, the session limit is 4096 and the stream limit is 8448. stream-up holds two long-running requests per session, stream-one uses one, and packet-up uses a downlink plus finite upload requests. Explicit maxIncomingStreams values, including 3 and -1, are preserved: -1 disables incoming bidirectional streams. When setting a manual limit, allow room for requests used by the selected mode; QUIC streams and sessions are counted separately.

uTLS changes ClientHello, not the entire browser stack. The partial Chrome 133 and 155 HTTP/2 profiles require their matching TLS profile; other profiles use the standard HTTP/2 client. Chrome 155 SETTINGS, connection window and pseudoheader order were compared with official Chrome for Testing 155.0.8059.84, revision 1697595; the browser's complete network behavior was not compared. HTTP headers and HTTP/2 settings are visible to a TLS terminator, not automatically to an intermediate ISP. TLS record sizes, traffic volume, direction and timing remain observable. Padding does not guarantee browser-like traffic or defeat an unknown classifier. Such a claim requires comparative captures and false-positive measurements on the target network.

Core 1.1.21 removes Go/uTLS's initial TLS record-size ramp for the verified Chrome 133/155 H2 profiles. The write queue combines ready data without waiting for a batch: at most 64 KiB pending and 64 KiB active per connection. Default packet-up intervals vary from 20 to 40 ms, retaining the 30 ms midpoint; explicit scMinPostsIntervalMs is preserved. No background browsing requests are generated. This does not promise that every TLS record or TCP segment matches Chrome.

Explicit minVersion/maxVersion are validated and enforced with uTLS too. A fingerprint without a permitted version or an incompatible endpoint fails closed. Custom version bounds can change ClientHello. ECH and HTTP/3 require TLS 1.3.

REALITY retains its existing fallback/spider: an ordinary trusted certificate instead of REALITY authentication can trigger HTTPS requests to the configured server. This does not grant VPN access. The statement about no added background requests concerns Xera itself.

### Selecting a mode and understanding circumvention limits

Research was checked as of 14 November 2026. Measurements in China and Russia concern different mechanisms: IP/SNI filtering, QUIC Initial inspection, active probing and transfer limits after connection establishment. Publication dates do not establish continuing deployment on every network. The reported “AI detector” has no separately verified technical description, dataset or detection criteria here; Xera has no demonstrated protection against an unknown classifier.

| Conditions | Configuration and verification |
| --- | --- |
| Direct TCP connection | Correctly configured REALITY and `stream-auto`. REALITY checks its admission before HTTP; VLESS authenticates the user afterwards. Check the target and actual endpoint reachability. |
| CDN or proxy buffering uploads | TLS with `packet-up` or `stream-auto`, which selects finite POSTs here. Verify data delivery, not just TLS establishment. Ordinary CDN TLS termination cannot preserve an unchanged REALITY handshake. |
| UDP is reachable and HTTP/3 is required | A separate TLS profile with `alpn: ["h3"]`. Verify MTU, sustained transfer and idle recovery. QUIC Initial alone does not hide SNI; actual ECH negotiation is required if the server supports it. |
| UDP is blocked | A separate TCP HTTP/2 profile on a reachable endpoint. Xera does not migrate an open H3 stream to H2 automatically. Another mode on the same blocked IP does not create another route. |
| Transfer stops after a small amount of data | Test sustained upload and download, including nested HTTPS. HTTP request size is not total TCP connection volume. Splitting POSTs does not guarantee evasion of a connection-wide limit. |
| Strict destination allowlist | An admitted, reachable path is required first. Padding and ClientHello cannot recover packets discarded before TLS. |

A CDN terminating TLS can inspect HTTP and tunnel contents after removing outer TLS and padding. Without separate VLESS encryption this includes VLESS metadata; nested HTTPS retains its own protection. Padding is neither encryption nor protection against the CDN itself.

Use an IPv6 literal in `address`, for example `2001:db8::1`; URLs and address:port pairs require brackets: `[2001:db8::1]:443`. This example uses a reserved documentation address. Set SNI to a name matching the TLS/REALITY configuration. Core IPv6 support does not provision a host or Docker IPv6 route or override operating-system restrictions. Check IPv4 and IPv6 independently, including a separate `downloadSettings` address. DNS, address family and exit country are different properties. Client autoselect must test the selected address from the client's network rather than rely on panel-side reachability.

### Browser Dialer and ECH

Browser Dialer uses a real browser and requires a separate helper page to remain open. Bind it to loopback and open its configured address directly. The page and WebSocket check Host/Origin, do not expose their token to arbitrary websites, disable page caching and prevent framing. Reload an existing helper page after updating Core. These controls protect against foreign webpages, not a process that already has local access.

Fetch does not follow redirects, cache tunnel responses or forward HTTP error bodies as stream data. Place Browser Dialer session/sequence/padding metadata in paths, queries or headers, and payloads in bodies or headers. The helper cannot set another origin's cookies: cookie placements and explicit Cookie headers fail before connecting. GET/HEAD bodies and Fetch-forbidden methods also fail early. Native clients retain cookie placement support; `customDownlinkPadding` remains incompatible with Browser Dialer.

ECH hides the inner ClientHello only when negotiation succeeds. The IP, outer SNI and traffic characteristics remain observable. ECH configuration acquisition is a separate dependency: `udp://` exposes the DNS query, while the selected DoH endpoint must be reachable from the target network. Large HTTPS/ECH records are supported; truncated UDP answers retry over TCP at the same resolver with the same socket settings. DoH answers are capped at 65535 bytes and checked before caching. A DoH failure does not trigger plaintext DNS fallback. An ECH failure does not authorize silent removal of ECH or certificate verification.

Primary research on these limits: [TLS-in-TLS fingerprinting](https://www.usenix.org/conference/usenixsecurity24/presentation/xue-fingerprinting), [The Parrot Is Dead](https://dedis.cs.yale.edu/dissent/papers/parrot-abs/). These studies are not measurements of Xera.

Additional primary references: [China QUIC-SNI measurements, 2025](https://gfw.report/publications/usenixsecurity25/en/), [Russia QUIC-SNI measurements, March–April 2026](https://www.petsymposium.org/foci/2026/foci-2026-0010.pdf), [Cloudflare's Russia transfer-limit report, June 2025](https://blog.cloudflare.com/russian-internet-users-are-unable-to-access-the-open-internet/), [ECH measurements, March 2025](https://petsymposium.org/foci/2025/foci-2025-0016.pdf), [RFC 9849](https://www.rfc-editor.org/rfc/rfc9849.html), [RFC 8484 §6](https://www.rfc-editor.org/rfc/rfc8484.html#section-6). Selected background reading: Ross Anderson, [Security Engineering, chapter 23, §§23.3.1–23.3.3](https://www.cl.cam.ac.uk/archive/rja14/Papers/SEv3-ch23.pdf). Historical observations and laboratory tests do not establish November 2026 availability across either country.
