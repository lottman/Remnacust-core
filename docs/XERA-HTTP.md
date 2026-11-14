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

uTLS согласует ClientHello, но не превращает весь клиент в браузер. Частичные HTTP/2-профили Chrome 133 и 155 применяются только с соответствующим TLS-профилем; остальные профили используют стандартный HTTP/2-клиент. Для Chrome 155 сравнивались SETTINGS, окно соединения и порядок псевдозаголовков с официальным Chrome for Testing 155.0.8059.84, revision 1697595. Это не сравнение всего сетевого поведения браузера. Параметры HTTP/2 и HTTP-заголовки видны TLS-терминатору, а не автоматически любому промежуточному провайдеру. Длины TLS-записей, объём, направление и интервалы передачи остаются наблюдаемыми. Padding не гарантирует неотличимость от просмотра сайтов. Проверка против конкретного классификатора требует сравнительных захватов и измерения ложных срабатываний в целевой сети.

В Core 1.1.20 проверенные H2-профили Chrome 133/155 не используют начальный рост TLS-записей Go/uTLS. Очередь объединяет готовые данные без ожидания заполнения: до 64 КиБ pending и 64 КиБ active на соединение. Паузы packet-up по умолчанию выбираются из 20–40 мс; явный scMinPostsIntervalMs сохраняется. Новых фоновых запросов для имитации веб-сёрфинга нет. Это не обещание совпадения всех TLS-записей или TCP-сегментов с Chrome.

Явные minVersion/maxVersion проверяются и соблюдаются также при uTLS. Отпечаток без подходящей версии или несовместимый сервер отклоняется. Пользовательские ограничения версий могут изменить ClientHello. ECH и HTTP/3 требуют TLS 1.3.

У REALITY сохранён собственный fallback/spider: обычный доверенный сертификат вместо подтверждения REALITY может привести к HTTPS-запросам к настроенному серверу. Это не подтверждает доступ к VPN. Утверждение об отсутствии новых фоновых запросов относится к самому Xera.

## Deployment

Configure `network: "xera-http"` and `xeraHttpSettings` on compatible client and server cores. VLESS authenticates the UUID after HTTP stream admission. A valid path, session ID or tokenish padding is not a credential; another client can construct these values.

Use REALITY or matching `customDownlinkPadding` with an unpredictable profile token to reject requests that do not know the profile settings. The JSON above belongs inside `xeraHttpSettings`. Generate the token from 32 random bytes, for example with the OpenSSL command above; never use the placeholder. Additional padding changes the data framing, so enabling it only on the server breaks older clients.

The token is shared with profile clients and visible to a TLS-terminating CDN. Keep it out of logs and public examples. Missing, incorrect or duplicate markers return the same ordinary 404 before body reads or session allocation. Browser Dialer is unsupported with this extension. The static marker does not prevent replay after disclosure and does not replace VLESS credentials, certificate verification or REALITY authentication.

`serverMaxSessions`, `serverMaxBufferedBytes` and `serverMaxHeaderBytes` bound different resources. Failed admission immediately releases a new session unless another request is using it or a valid upload has already been accepted. Successful packet-up sessions retain their 30-second downlink wait; individual packet-body reads have a separate deadline. An invalid request does not delete an accepted session. HTTP/3 also enforces the configured header limit; accounting for protocol metadata differs between HTTP versions.

uTLS changes ClientHello, not the entire browser stack. The partial Chrome 133 and 155 HTTP/2 profiles require their matching TLS profile; other profiles use the standard HTTP/2 client. Chrome 155 SETTINGS, connection window and pseudoheader order were compared with official Chrome for Testing 155.0.8059.84, revision 1697595; the browser's complete network behavior was not compared. HTTP headers and HTTP/2 settings are visible to a TLS terminator, not automatically to an intermediate ISP. TLS record sizes, traffic volume, direction and timing remain observable. Padding does not guarantee browser-like traffic or defeat an unknown classifier. Such a claim requires comparative captures and false-positive measurements on the target network.

Core 1.1.20 removes Go/uTLS's initial TLS record-size ramp for the verified Chrome 133/155 H2 profiles. The write queue combines ready data without waiting for a batch: at most 64 KiB pending and 64 KiB active per connection. Default packet-up intervals vary from 20 to 40 ms, retaining the 30 ms midpoint; explicit scMinPostsIntervalMs is preserved. No background browsing requests are generated. This does not promise that every TLS record or TCP segment matches Chrome.

Explicit minVersion/maxVersion are validated and enforced with uTLS too. A fingerprint without a permitted version or an incompatible endpoint fails closed. Custom version bounds can change ClientHello. ECH and HTTP/3 require TLS 1.3.

REALITY retains its existing fallback/spider: an ordinary trusted certificate instead of REALITY authentication can trigger HTTPS requests to the configured server. This does not grant VPN access. The statement about no added background requests concerns Xera itself.

Primary research on these limits: [TLS-in-TLS fingerprinting](https://www.usenix.org/conference/usenixsecurity24/presentation/xue-fingerprinting), [The Parrot Is Dead](https://dedis.cs.yale.edu/dissent/papers/parrot-abs/). These studies are not measurements of Xera.
