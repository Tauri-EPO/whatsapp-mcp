# Changelog

## [1.5.1](https://github.com/Tauri-EPO/whatsapp-mcp/compare/v1.5.0...v1.5.1) (2026-09-09)


### Bug fixes

* **bridge:** the store is opened through os.Root, so a symlink cannot lead a delete or a read outside it ([#419](https://github.com/Tauri-EPO/whatsapp-mcp/issues/419)) ([42bbd43](https://github.com/Tauri-EPO/whatsapp-mcp/commit/42bbd430dd4caf63c082d1907256f4332e291c0f))
* **scripts:** smoke.sh catches a whisper container orphaned by a redeploy ([#416](https://github.com/Tauri-EPO/whatsapp-mcp/issues/416)) ([18e6d83](https://github.com/Tauri-EPO/whatsapp-mcp/commit/18e6d831a37d27479e7a4f853b3998f206d6526a)), closes [#415](https://github.com/Tauri-EPO/whatsapp-mcp/issues/415)

## [1.5.0](https://github.com/Tauri-EPO/whatsapp-mcp/compare/v1.4.0...v1.5.0) (2026-09-08)


### Features

* **scripts:** smoke.sh works against manager-owned stacks ([#384](https://github.com/Tauri-EPO/whatsapp-mcp/issues/384)) ([cc2337a](https://github.com/Tauri-EPO/whatsapp-mcp/commit/cc2337a0f996a5fe915b48acf47152b690d13e90)), closes [#380](https://github.com/Tauri-EPO/whatsapp-mcp/issues/380)


### Bug fixes

* a media row with no key is a permanent miss, not a retry ([#401](https://github.com/Tauri-EPO/whatsapp-mcp/issues/401)) ([e3b27ba](https://github.com/Tauri-EPO/whatsapp-mcp/commit/e3b27ba0beff81edcf858b86717ab8414c1d9798)), closes [#392](https://github.com/Tauri-EPO/whatsapp-mcp/issues/392)
* audio the sender's phone no longer has is recorded, not re-fetched ([#391](https://github.com/Tauri-EPO/whatsapp-mcp/issues/391)) ([18ff6bc](https://github.com/Tauri-EPO/whatsapp-mcp/commit/18ff6bc04b75aa60221effa0432d0c137619fb29)), closes [#378](https://github.com/Tauri-EPO/whatsapp-mcp/issues/378)
* **bridge:** gofmt group_members_test.go so the lint gate is green on main ([#405](https://github.com/Tauri-EPO/whatsapp-mcp/issues/405)) ([0097a14](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0097a14b64b0f300ada44721b86c1d7937cf258a))
* **bridge:** record which namespace a stored sender belongs to ([#383](https://github.com/Tauri-EPO/whatsapp-mcp/issues/383)) ([aac47c2](https://github.com/Tauri-EPO/whatsapp-mcp/commit/aac47c20161bce864e5704ca8f3f041688a7404f)), closes [#375](https://github.com/Tauri-EPO/whatsapp-mcp/issues/375)
* **bridge:** the group owner comes back dual-addressed, like a participant ([#400](https://github.com/Tauri-EPO/whatsapp-mcp/issues/400)) ([f1c589e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/f1c589eaa0a9b5da0b43e5ea6d7df102bc96e5fb)), closes [#396](https://github.com/Tauri-EPO/whatsapp-mcp/issues/396)
* gofmt -s on that one file (two lines of alignment), nothing else. ([0097a14](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0097a14b64b0f300ada44721b86c1d7937cf258a))
* **mcp:** "ok [@you](https://github.com/you)" is a closing message for the ordinary rule too ([#414](https://github.com/Tauri-EPO/whatsapp-mcp/issues/414)) ([ef40edf](https://github.com/Tauri-EPO/whatsapp-mcp/commit/ef40edffee12f349ec7f33d5751ab2b79a848545)), closes [#411](https://github.com/Tauri-EPO/whatsapp-mcp/issues/411)
* **mcp:** a mention that only says "ok" no longer counts as waiting ([#404](https://github.com/Tauri-EPO/whatsapp-mcp/issues/404)) ([b198e70](https://github.com/Tauri-EPO/whatsapp-mcp/commit/b198e70d12c521d02e4b1bc1af0893402831ecc9)), closes [#395](https://github.com/Tauri-EPO/whatsapp-mcp/issues/395)
* **mcp:** a third party's "ok" no longer hides a pending mention ([#410](https://github.com/Tauri-EPO/whatsapp-mcp/issues/410)) ([40f0ef3](https://github.com/Tauri-EPO/whatsapp-mcp/commit/40f0ef3fd6073ee4df9bea726c4012e44d22680f))
* **mcp:** a whisper outage no longer parks a voice note for ever ([#386](https://github.com/Tauri-EPO/whatsapp-mcp/issues/386)) ([bc0a10d](https://github.com/Tauri-EPO/whatsapp-mcp/commit/bc0a10d16c7fb12a9244267d3ac49ac12c8bb6e5)), closes [#377](https://github.com/Tauri-EPO/whatsapp-mcp/issues/377)
* **mcp:** py-modules lists every module, and the tree test keeps it that way ([#403](https://github.com/Tauri-EPO/whatsapp-mcp/issues/403)) ([fdfd952](https://github.com/Tauri-EPO/whatsapp-mcp/commit/fdfd952d3dcd18f43b9c3734baa1bd5cddf9fd0d)), closes [#398](https://github.com/Tauri-EPO/whatsapp-mcp/issues/398)
* **mcp:** status@broadcast is the status feed, not a conversation ([#387](https://github.com/Tauri-EPO/whatsapp-mcp/issues/387)) ([b68c447](https://github.com/Tauri-EPO/whatsapp-mcp/commit/b68c4475ce912f84761ed812a5a488574a25c6c7))
* **mcp:** the sender's namespace comes from the store, not from its length ([#389](https://github.com/Tauri-EPO/whatsapp-mcp/issues/389)) ([a48e857](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a48e85749fb1eb43c5ca0010e5bd0156980ac405)), closes [#375](https://github.com/Tauri-EPO/whatsapp-mcp/issues/375)


### Refactoring

* **bridge:** timing knobs live on Bridge, testBridge drains its own pool ([#390](https://github.com/Tauri-EPO/whatsapp-mcp/issues/390)) ([971ce81](https://github.com/Tauri-EPO/whatsapp-mcp/commit/971ce819eb9ff88d4178e4188c47b64bd721154b)), closes [#382](https://github.com/Tauri-EPO/whatsapp-mcp/issues/382)

## [1.4.0](https://github.com/Tauri-EPO/whatsapp-mcp/compare/v1.3.0...v1.4.0) (2026-09-08)


### Features

* **mcp:** every attachment is an MCP resource, and list_media links to it ([#372](https://github.com/Tauri-EPO/whatsapp-mcp/issues/372)) ([2b8bd89](https://github.com/Tauri-EPO/whatsapp-mcp/commit/2b8bd89e740782b23d3fa7793521d615c182c75e)), closes [#367](https://github.com/Tauri-EPO/whatsapp-mcp/issues/367)
* **mcp:** read_media downscales images for the model ([#374](https://github.com/Tauri-EPO/whatsapp-mcp/issues/374)) ([3ab583d](https://github.com/Tauri-EPO/whatsapp-mcp/commit/3ab583d44e528ad33c3917e655f994f3021b0244))
* **mcp:** read_media renders PDF pages as images ([#376](https://github.com/Tauri-EPO/whatsapp-mcp/issues/376)) ([118113f](https://github.com/Tauri-EPO/whatsapp-mcp/commit/118113fb7c3cad9c3806fb13715a1be401366ce1)), closes [#368](https://github.com/Tauri-EPO/whatsapp-mcp/issues/368)
* **mcp:** read_media returns non-image media as typed content blocks ([#370](https://github.com/Tauri-EPO/whatsapp-mcp/issues/370)) ([9306b96](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9306b96f0738e2a6a098dc742223e5c4e21920a0))


### Bug fixes

* **mcp:** one triage row for a contact known under both spellings ([#373](https://github.com/Tauri-EPO/whatsapp-mcp/issues/373)) ([9ffe1fa](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9ffe1fab8838c33aef7cee7c668ca33684da0171)), closes [#366](https://github.com/Tauri-EPO/whatsapp-mcp/issues/366)
* **mcp:** the group-mention stream obeys the triage notes too ([#369](https://github.com/Tauri-EPO/whatsapp-mcp/issues/369)) ([ded3696](https://github.com/Tauri-EPO/whatsapp-mcp/commit/ded36968656519df9a8ce3429ee460938121ac38)), closes [#361](https://github.com/Tauri-EPO/whatsapp-mcp/issues/361)

## [1.3.0](https://github.com/Tauri-EPO/whatsapp-mcp/compare/v1.2.0...v1.3.0) (2026-09-08)


### Features

* **bridge:** bound and cancel automatic media downloads ([#348](https://github.com/Tauri-EPO/whatsapp-mcp/issues/348)) ([4c5ed3b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4c5ed3bccaef2832eefc495a1f3cfd39ef2d862a)), closes [#320](https://github.com/Tauri-EPO/whatsapp-mcp/issues/320)
* **bridge:** cache group rosters in a group_members table ([#327](https://github.com/Tauri-EPO/whatsapp-mcp/issues/327)) ([3f51ce6](https://github.com/Tauri-EPO/whatsapp-mcp/commit/3f51ce6721a72db756b7f9dd7c4b2fd7f37fd43b))
* **bridge:** mark a whole chat read without listing message IDs ([#304](https://github.com/Tauri-EPO/whatsapp-mcp/issues/304)) ([ff2745f](https://github.com/Tauri-EPO/whatsapp-mcp/commit/ff2745f464cc2398a64d4d9cff2d126e81e2b996)), closes [#291](https://github.com/Tauri-EPO/whatsapp-mcp/issues/291)
* **bridge:** store mentions and expose the owner identity ([#326](https://github.com/Tauri-EPO/whatsapp-mcp/issues/326)) ([952afc8](https://github.com/Tauri-EPO/whatsapp-mcp/commit/952afc81446fa86854828e9120d7484ffce89064))
* **mcp:** a per-tool latency histogram on /metrics ([#359](https://github.com/Tauri-EPO/whatsapp-mcp/issues/359)) ([0bbb1eb](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0bbb1ebc0ee7ee34b2506d5053ca08c9a11d9155)), closes [#322](https://github.com/Tauri-EPO/whatsapp-mcp/issues/322)
* **mcp:** bridge_status reports the published endpoint certificate expiry ([#300](https://github.com/Tauri-EPO/whatsapp-mcp/issues/300)) ([a2e54d7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a2e54d73a45ec7285c2571aef6ade95376531335)), closes [#277](https://github.com/Tauri-EPO/whatsapp-mcp/issues/277)
* **mcp:** bridge_status reports whether whisper is available ([#295](https://github.com/Tauri-EPO/whatsapp-mcp/issues/295)) ([1a99299](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1a992993b10b3fd76353f7f7f1e6ca89db3717bd)), closes [#284](https://github.com/Tauri-EPO/whatsapp-mcp/issues/284)
* **mcp:** chat_jid takes a list, exclude_chat_jid mirrors it, message_stats counts a query ([#303](https://github.com/Tauri-EPO/whatsapp-mcp/issues/303)) ([40320d4](https://github.com/Tauri-EPO/whatsapp-mcp/commit/40320d451e85db6e3caae87cfdce79139c3ef1db)), closes [#289](https://github.com/Tauri-EPO/whatsapp-mcp/issues/289)
* **mcp:** coverage reports the voice-note transcription backlog ([#363](https://github.com/Tauri-EPO/whatsapp-mcp/issues/363)) ([42141f4](https://github.com/Tauri-EPO/whatsapp-mcp/commit/42141f43270df0a747c1fe6604dc0936a989fdb8)), closes [#334](https://github.com/Tauri-EPO/whatsapp-mcp/issues/334)
* **mcp:** coverage takes a window and lists which chats to backfill ([#311](https://github.com/Tauri-EPO/whatsapp-mcp/issues/311)) ([56c900c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/56c900c6ad9b7407691020e814a0e5e1d96be438))
* **mcp:** get_contact_chats includes the groups a contact belongs to ([#340](https://github.com/Tauri-EPO/whatsapp-mcp/issues/340)) ([8aad048](https://github.com/Tauri-EPO/whatsapp-mcp/commit/8aad0480229db03ca618d4dc2bdfe37fcf410b44)), closes [#288](https://github.com/Tauri-EPO/whatsapp-mcp/issues/288)
* **mcp:** mark a chat handled or snoozed so list_unanswered stops repeating it ([#328](https://github.com/Tauri-EPO/whatsapp-mcp/issues/328)) ([52d1213](https://github.com/Tauri-EPO/whatsapp-mcp/commit/52d1213498089dc7c2ec9b33fc2188438f42f8b1)), closes [#287](https://github.com/Tauri-EPO/whatsapp-mcp/issues/287)
* **mcp:** mentions_me filter and group mentions in triage ([#330](https://github.com/Tauri-EPO/whatsapp-mcp/issues/330)) ([d763f48](https://github.com/Tauri-EPO/whatsapp-mcp/commit/d763f4851b6d238df4486a10685205b6d19e8fca))
* **mcp:** notes for chats, contacts and messages ([#306](https://github.com/Tauri-EPO/whatsapp-mcp/issues/306)) ([58319eb](https://github.com/Tauri-EPO/whatsapp-mcp/commit/58319eb71c5385677c1b44475220e02acac3e757))
* **mcp:** notes inline in listings, compact for append keys ([#308](https://github.com/Tauri-EPO/whatsapp-mcp/issues/308)) ([b473bf5](https://github.com/Tauri-EPO/whatsapp-mcp/commit/b473bf5cfa3b4c3ac4dd7a28d4158437c7badc74)), closes [#286](https://github.com/Tauri-EPO/whatsapp-mcp/issues/286)
* **mcp:** rank audio hits with an FTS5 index over transcripts ([#299](https://github.com/Tauri-EPO/whatsapp-mcp/issues/299)) ([85b5a73](https://github.com/Tauri-EPO/whatsapp-mcp/commit/85b5a73e00e64a6f885e3622167a7bbf39e068b0)), closes [#275](https://github.com/Tauri-EPO/whatsapp-mcp/issues/275)
* **mcp:** read_media returns the file's bytes, not a server path ([#325](https://github.com/Tauri-EPO/whatsapp-mcp/issues/325)) ([217365f](https://github.com/Tauri-EPO/whatsapp-mcp/commit/217365fad3df6f70d4104e2b8f8fe1ca4f2ce0fc))
* **mcp:** read_media(as_text=True) reads PDF, DOCX and XLSX as text ([#329](https://github.com/Tauri-EPO/whatsapp-mcp/issues/329)) ([6f6863c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/6f6863c536d8054d1a0db8053ac2170f8da04305))
* **mcp:** return the name a contact gave themselves beside the one you saved ([#310](https://github.com/Tauri-EPO/whatsapp-mcp/issues/310)) ([9ac3ffa](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9ac3ffa5c0301d9b2d101abd7564ed98e4f9a4f0)), closes [#280](https://github.com/Tauri-EPO/whatsapp-mcp/issues/280)
* **mcp:** sanitise name fields instead of wrapping them ([#302](https://github.com/Tauri-EPO/whatsapp-mcp/issues/302)) ([4cf4c12](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4cf4c12136a87adb5deabb13f0c121dd124700c6)), closes [#273](https://github.com/Tauri-EPO/whatsapp-mcp/issues/273)
* **mcp:** TRANSCRIBE_ON_INGEST can fetch uncached voice notes ([#296](https://github.com/Tauri-EPO/whatsapp-mcp/issues/296)) ([3f52ae0](https://github.com/Tauri-EPO/whatsapp-mcp/commit/3f52ae0425cbf436548ceb60c560ea2c0aefc79c)), closes [#276](https://github.com/Tauri-EPO/whatsapp-mcp/issues/276)
* **mcp:** triage filters on list_unread, min_messages on list_unanswered ([#358](https://github.com/Tauri-EPO/whatsapp-mcp/issues/358)) ([4fb46cc](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4fb46cce7b10f43707f9179f384f604c97aeea8c))


### Bug fixes

* **bridge:** one canonical UTC spelling for every stored timestamp ([#297](https://github.com/Tauri-EPO/whatsapp-mcp/issues/297)) ([2e525da](https://github.com/Tauri-EPO/whatsapp-mcp/commit/2e525dab13d5df69a9c0d5b877c69933aa56076c))
* **bridge:** one media transfer per destination file ([#345](https://github.com/Tauri-EPO/whatsapp-mcp/issues/345)) ([b9a1a35](https://github.com/Tauri-EPO/whatsapp-mcp/commit/b9a1a3569d08e4ba102bea31bd3dbf6b8ee282b9)), closes [#319](https://github.com/Tauri-EPO/whatsapp-mcp/issues/319)
* **bridge:** repair timestamps an older bridge wrote after the migration ([#352](https://github.com/Tauri-EPO/whatsapp-mcp/issues/352)) ([9a651ab](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9a651ab654acdfd60eedaf490f53763aa134aced)), closes [#335](https://github.com/Tauri-EPO/whatsapp-mcp/issues/335)
* **bridge:** the StreamReplaced delay is a Bridge field, and CI runs -race ([#365](https://github.com/Tauri-EPO/whatsapp-mcp/issues/365)) ([a2669e4](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a2669e4051ce7129907545613610e38d3026b147))
* **mcp:** a page size below 1 is refused instead of paging forever ([#356](https://github.com/Tauri-EPO/whatsapp-mcp/issues/356)) ([da8d2e6](https://github.com/Tauri-EPO/whatsapp-mcp/commit/da8d2e65e1d758641cc55dd036211de264e450ad)), closes [#309](https://github.com/Tauri-EPO/whatsapp-mcp/issues/309)
* **mcp:** denying download_media stops the implicit fetches too ([#362](https://github.com/Tauri-EPO/whatsapp-mcp/issues/362)) ([97a9e7e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/97a9e7e48ce4ce99fe5af61805d4039f32b76d1b)), closes [#350](https://github.com/Tauri-EPO/whatsapp-mcp/issues/350)
* **mcp:** every matching transcript is a candidate, so a scoped count is exact ([#343](https://github.com/Tauri-EPO/whatsapp-mcp/issues/343)) ([ba90a03](https://github.com/Tauri-EPO/whatsapp-mcp/commit/ba90a034346fd6e666a0a01c66cae689a34c42d9)), closes [#316](https://github.com/Tauri-EPO/whatsapp-mcp/issues/316)
* **mcp:** keep LIDs out of the phone-number fields ([#305](https://github.com/Tauri-EPO/whatsapp-mcp/issues/305)) ([34401ab](https://github.com/Tauri-EPO/whatsapp-mcp/commit/34401abdc625d4b82c23331e09992519c2db4cf1)), closes [#281](https://github.com/Tauri-EPO/whatsapp-mcp/issues/281)
* **mcp:** note searches walk their matches in bounded batches ([#353](https://github.com/Tauri-EPO/whatsapp-mcp/issues/353)) ([5887c24](https://github.com/Tauri-EPO/whatsapp-mcp/commit/5887c2420f9bd6f29c8b72f8f494ca435c9db02c)), closes [#315](https://github.com/Tauri-EPO/whatsapp-mcp/issues/315)
* **mcp:** one listing row for a contact known under both spellings ([#364](https://github.com/Tauri-EPO/whatsapp-mcp/issues/364)) ([f056003](https://github.com/Tauri-EPO/whatsapp-mcp/commit/f056003616683ec6bb6f2bcfb6ce90c1bfe42369))
* **mcp:** page messages on the full store key so forwarded copies are not skipped ([#342](https://github.com/Tauri-EPO/whatsapp-mcp/issues/342)) ([c0d5c7a](https://github.com/Tauri-EPO/whatsapp-mcp/commit/c0d5c7a2875a7e7cccdc6f3905e8172d2063d48c)), closes [#313](https://github.com/Tauri-EPO/whatsapp-mcp/issues/313)
* **mcp:** sanitise and wrap the group topic and the poll question ([#360](https://github.com/Tauri-EPO/whatsapp-mcp/issues/360)) ([a1540f6](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a1540f60db31244b0d066697188ba4470236706f)), closes [#332](https://github.com/Tauri-EPO/whatsapp-mcp/issues/332)
* **mcp:** shape list_chats, and refuse arguments no tool declares ([#307](https://github.com/Tauri-EPO/whatsapp-mcp/issues/307)) ([15acb59](https://github.com/Tauri-EPO/whatsapp-mcp/commit/15acb590bc09618ac6295f47083dbfb9553459cd)), closes [#282](https://github.com/Tauri-EPO/whatsapp-mcp/issues/282)
* **mcp:** the ingest walk advances past audio it cannot read ([#347](https://github.com/Tauri-EPO/whatsapp-mcp/issues/347)) ([1194e74](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1194e7473359f70dcbf744a345cb52bad8491ad4)), closes [#314](https://github.com/Tauri-EPO/whatsapp-mcp/issues/314)
* **mcp:** the ingest worker obeys the tool policy ([#349](https://github.com/Tauri-EPO/whatsapp-mcp/issues/349)) ([d14b249](https://github.com/Tauri-EPO/whatsapp-mcp/commit/d14b24998959072a11bf8546db20f27ebebd479c)), closes [#331](https://github.com/Tauri-EPO/whatsapp-mcp/issues/331)


### Performance

* **mcp:** context windows seek the neighbours instead of ranking the chat ([#344](https://github.com/Tauri-EPO/whatsapp-mcp/issues/344)) ([a5731e7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a5731e720e6ffd1e5c20872c31cee795ccb947c3)), closes [#312](https://github.com/Tauri-EPO/whatsapp-mcp/issues/312)
* **mcp:** count a media page's copies instead of the whole archive ([#354](https://github.com/Tauri-EPO/whatsapp-mcp/issues/354)) ([81f0379](https://github.com/Tauri-EPO/whatsapp-mcp/commit/81f037979840ebfb6934c38ff690a711ad5a1685)), closes [#317](https://github.com/Tauri-EPO/whatsapp-mcp/issues/317)
* **mcp:** find a transcript's index entry by rowid instead of reading the index ([#346](https://github.com/Tauri-EPO/whatsapp-mcp/issues/346)) ([145bb5b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/145bb5b132a4d691d4c1a9dcff458db07d77e72a)), closes [#321](https://github.com/Tauri-EPO/whatsapp-mcp/issues/321)
* **mcp:** look a cached file up by name instead of scanning the chat ([#357](https://github.com/Tauri-EPO/whatsapp-mcp/issues/357)) ([4dbfd3d](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4dbfd3d6d3fd0d13ea6c0b891c8eabd9a9101cf0)), closes [#318](https://github.com/Tauri-EPO/whatsapp-mcp/issues/318)

## [1.2.0](https://github.com/Tauri-EPO/whatsapp-mcp/compare/v1.1.0...v1.2.0) (2026-09-07)


### Features

* **bridge:** enforce WHATSAPP_ALLOW_TOOLS / _DENY_TOOLS on the REST endpoints ([#264](https://github.com/Tauri-EPO/whatsapp-mcp/issues/264)) ([8c829b9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/8c829b9220cd26a53bf889d73bf3b6f660c58cc2)), closes [#255](https://github.com/Tauri-EPO/whatsapp-mcp/issues/255)
* exclude_groups keeps direct conversations only ([#263](https://github.com/Tauri-EPO/whatsapp-mcp/issues/263)) ([c1e2336](https://github.com/Tauri-EPO/whatsapp-mcp/commit/c1e2336487046ebfcd4afbdc93f717de6856b0b9)), closes [#256](https://github.com/Tauri-EPO/whatsapp-mcp/issues/256)
* **mcp:** list_messages(query=...) also matches stored voice-note transcripts ([#267](https://github.com/Tauri-EPO/whatsapp-mcp/issues/267)) ([3ffa34e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/3ffa34ee88fefabfc1250f036dde91f63982c99c)), closes [#254](https://github.com/Tauri-EPO/whatsapp-mcp/issues/254)
* **mcp:** transcribe inbound voice notes in the background (TRANSCRIBE_ON_INGEST) ([#265](https://github.com/Tauri-EPO/whatsapp-mcp/issues/265)) ([21f7d9d](https://github.com/Tauri-EPO/whatsapp-mcp/commit/21f7d9d6da1ff054eb51bdf98e2691acf430eaf9))


### Bug fixes

* compare after/before against a normalised timestamp in every message query ([#266](https://github.com/Tauri-EPO/whatsapp-mcp/issues/266)) ([14841c2](https://github.com/Tauri-EPO/whatsapp-mcp/commit/14841c2a7486d5d619e29751e82b08bf7b5447c5)), closes [#253](https://github.com/Tauri-EPO/whatsapp-mcp/issues/253)


### Refactoring

* one phone-book lookup, one age clock, one message-field list ([#269](https://github.com/Tauri-EPO/whatsapp-mcp/issues/269)) ([bf17757](https://github.com/Tauri-EPO/whatsapp-mcp/commit/bf17757c774fb2ea0acfa077b3f47b502884d2fd)), closes [#257](https://github.com/Tauri-EPO/whatsapp-mcp/issues/257)


### Dependencies

* bump ggml-org/whisper.cpp from main to main ([#262](https://github.com/Tauri-EPO/whatsapp-mcp/issues/262)) ([ed4ce35](https://github.com/Tauri-EPO/whatsapp-mcp/commit/ed4ce35648c54ec89a7e3b4743b39809994bb7a9))

## [1.1.0](https://github.com/Tauri-EPO/whatsapp-mcp/compare/v1.0.0...v1.1.0) (2026-09-07)


### Features

* cache voice-note transcripts in notes.db and expose them in list_messages ([#246](https://github.com/Tauri-EPO/whatsapp-mcp/issues/246)) ([4511df7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4511df74feef674de05a6af9748630b07520173f)), closes [#221](https://github.com/Tauri-EPO/whatsapp-mcp/issues/221)
* compact responses for bulk reads (fields, omit_nulls, max_content_chars, count_only) ([#252](https://github.com/Tauri-EPO/whatsapp-mcp/issues/252)) ([159de8e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/159de8efdc453e6a9fa91000f3c707a05fca730f)), closes [#223](https://github.com/Tauri-EPO/whatsapp-mcp/issues/223)
* coverage tool reporting archive extent and sync gaps ([#244](https://github.com/Tauri-EPO/whatsapp-mcp/issues/244)) ([fec27af](https://github.com/Tauri-EPO/whatsapp-mcp/commit/fec27afd22fcffaf778ee5cf61c547b254de742a)), closes [#229](https://github.com/Tauri-EPO/whatsapp-mcp/issues/229)
* dry_run on send_message, send_file, edit_message ([#250](https://github.com/Tauri-EPO/whatsapp-mcp/issues/250)) ([3c43cda](https://github.com/Tauri-EPO/whatsapp-mcp/commit/3c43cda5818f0720a7c403812d37050429908df0)), closes [#232](https://github.com/Tauri-EPO/whatsapp-mcp/issues/232)
* export_messages streams the archive to NDJSON on disk ([#249](https://github.com/Tauri-EPO/whatsapp-mcp/issues/249)) ([372c649](https://github.com/Tauri-EPO/whatsapp-mcp/commit/372c649d3ac44463ad7c93968e3d8606609d10ce)), closes [#227](https://github.com/Tauri-EPO/whatsapp-mcp/issues/227)
* list_messages filters from_me, has_media, media_type, exclude_groups ([#235](https://github.com/Tauri-EPO/whatsapp-mcp/issues/235)) ([602628d](https://github.com/Tauri-EPO/whatsapp-mcp/commit/602628ddb2ad6cc4baeec0c9c6fb3a5eefee5ac6)), closes [#224](https://github.com/Tauri-EPO/whatsapp-mcp/issues/224)
* list_unanswered — chats where the other side spoke last ([#248](https://github.com/Tauri-EPO/whatsapp-mcp/issues/248)) ([9bd28e8](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9bd28e84a6bcf8cd44c0540a45534ec56e1093da)), closes [#219](https://github.com/Tauri-EPO/whatsapp-mcp/issues/219)
* list_unread exclude_groups and max_age_days ([#241](https://github.com/Tauri-EPO/whatsapp-mcp/issues/241)) ([82f3aa7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/82f3aa730c92b8b76578cefbb2251820249b5506)), closes [#225](https://github.com/Tauri-EPO/whatsapp-mcp/issues/225)
* mark message content as untrusted in tool descriptions and responses ([#251](https://github.com/Tauri-EPO/whatsapp-mcp/issues/251)) ([54edbf1](https://github.com/Tauri-EPO/whatsapp-mcp/commit/54edbf12e32c23b682ad0d4ba765068791467a0d)), closes [#231](https://github.com/Tauri-EPO/whatsapp-mcp/issues/231)
* message_stats tool for counts by chat, day, month or sender ([#243](https://github.com/Tauri-EPO/whatsapp-mcp/issues/243)) ([1cd6972](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1cd6972b897e28ac88e3193508a87f10091eaaef)), closes [#228](https://github.com/Tauri-EPO/whatsapp-mcp/issues/228)
* paginate list_group_members ([#236](https://github.com/Tauri-EPO/whatsapp-mcp/issues/236)) ([284bddb](https://github.com/Tauri-EPO/whatsapp-mcp/commit/284bddb3effe843684ecc9c20a6142dda731bcde)), closes [#226](https://github.com/Tauri-EPO/whatsapp-mcp/issues/226)
* request_history tool for on-demand history sync ([#239](https://github.com/Tauri-EPO/whatsapp-mcp/issues/239)) ([f1f6797](https://github.com/Tauri-EPO/whatsapp-mcp/commit/f1f67975407a0a592b12eb97d6af401b1a6c49ad)), closes [#220](https://github.com/Tauri-EPO/whatsapp-mcp/issues/220)
* return media notes wherever media is surfaced, and prompt the agent to annotate ([#238](https://github.com/Tauri-EPO/whatsapp-mcp/issues/238)) ([1d7abf6](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1d7abf658b2150b58b51ef3362cc6c46809cf674)), closes [#222](https://github.com/Tauri-EPO/whatsapp-mcp/issues/222)
* WHATSAPP_ALLOW_TOOLS / WHATSAPP_DENY_TOOLS per-tool allow/deny ([#247](https://github.com/Tauri-EPO/whatsapp-mcp/issues/247)) ([4109b6e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4109b6ec5d2baf30fbd7f1d07121c34e1076218e))
* WHATSAPP_READ_ONLY hides and refuses mutating tools (server + bridge) ([#242](https://github.com/Tauri-EPO/whatsapp-mcp/issues/242)) ([23176b7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/23176b772d91112a176e65cf004055c4da55cb0d))


### Bug fixes

* fall back to the phone book for chats stored without a name ([#245](https://github.com/Tauri-EPO/whatsapp-mcp/issues/245)) ([a185f63](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a185f631e7288686c1acbc3235eaa505c47a1635)), closes [#230](https://github.com/Tauri-EPO/whatsapp-mcp/issues/230)
* resolve list_chats' last message by ordering, not timestamp equality ([#237](https://github.com/Tauri-EPO/whatsapp-mcp/issues/237)) ([b2b813d](https://github.com/Tauri-EPO/whatsapp-mcp/commit/b2b813db281193e37d68eebfd9d24e46ee2d14f0)), closes [#218](https://github.com/Tauri-EPO/whatsapp-mcp/issues/218)

## [1.0.0](https://github.com/Tauri-EPO/whatsapp-mcp/compare/v0.6.0...v1.0.0) (2026-09-05)


### ⚠ BREAKING CHANGES

* **mcp:** the three tools return the page envelope instead of a bare list.
* **mcp:** failed tool calls return {"error": {...}} instead of {"success": false} / [] / None. Successful shapes are unchanged.
* **mcp:** tool parameter names and order changed; clients calling by name must update (see docs/TOOLS.md conventions).
* **mcp:** requires mcp>=2.1.1 and cryptography>=50; Intel macOS is no longer a supported install target for this fork.

### release

* adopt Release Please for automated versioning/changelog ([#15](https://github.com/Tauri-EPO/whatsapp-mcp/issues/15)) ([bef1a96](https://github.com/Tauri-EPO/whatsapp-mcp/commit/bef1a966ef254947f37833d06549665901133890))


### Features

* /api/version and build identity for both images ([#86](https://github.com/Tauri-EPO/whatsapp-mcp/issues/86)) ([52f6644](https://github.com/Tauri-EPO/whatsapp-mcp/commit/52f664423c63e824d27be262f6402fbc30f23db5)), closes [#56](https://github.com/Tauri-EPO/whatsapp-mcp/issues/56)
* add explicit message read receipts ([#201](https://github.com/Tauri-EPO/whatsapp-mcp/issues/201)) ([e35224e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/e35224e182fe06632ae0436949dda3cded544b17))
* add image media support in webhook forwarding ([#45](https://github.com/Tauri-EPO/whatsapp-mcp/issues/45)) ([43d7794](https://github.com/Tauri-EPO/whatsapp-mcp/commit/43d7794d8e003dc72fb9dba3b55eacee479753c8))
* **bridge:** --full-history-pair flag to request full history at pair time ([#37](https://github.com/Tauri-EPO/whatsapp-mcp/issues/37)) ([59b834f](https://github.com/Tauri-EPO/whatsapp-mcp/commit/59b834fcba2b0d9994d6a21946aae59ab65cff99))
* **bridge:** add macOS launchd installer ([#116](https://github.com/Tauri-EPO/whatsapp-mcp/issues/116)) ([dbc63ae](https://github.com/Tauri-EPO/whatsapp-mcp/commit/dbc63ae81b6c0d1e11fa9a06122522f1ce00e56b))
* **bridge:** add on-demand history sync for a single chat ([#168](https://github.com/Tauri-EPO/whatsapp-mcp/issues/168)) ([f44440b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/f44440b5874dba67c3daff2709458b2c45d7e1f7))
* **bridge:** add outbound webhook opt-out ([#204](https://github.com/Tauri-EPO/whatsapp-mcp/issues/204)) ([6c3f3fe](https://github.com/Tauri-EPO/whatsapp-mcp/commit/6c3f3fe2763584879166b5f92834cf7635d16a58))
* **bridge:** add support for stickers ([#110](https://github.com/Tauri-EPO/whatsapp-mcp/issues/110)) ([5d98686](https://github.com/Tauri-EPO/whatsapp-mcp/commit/5d98686b6007c768cefcbd314f2b2232724d0db4))
* **bridge:** capture incoming WhatsApp call events ([#39](https://github.com/Tauri-EPO/whatsapp-mcp/issues/39)) ([4f1664a](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4f1664a9e54858e0b48f1d417fd4aee9ea3bbf35))
* **bridge:** configurable linked-device name via WHATSAPP_DEVICE_NAME ([#157](https://github.com/Tauri-EPO/whatsapp-mcp/issues/157)) ([1a71032](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1a71032fd878c19eea346292f147350fadb4d818)), closes [#156](https://github.com/Tauri-EPO/whatsapp-mcp/issues/156)
* **bridge:** decode poll votes from history sync; report undecodable votes ([#92](https://github.com/Tauri-EPO/whatsapp-mcp/issues/92)) ([67d0ed7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/67d0ed793ac912cfd0d96303c591ef556c4c8c0d)), closes [#59](https://github.com/Tauri-EPO/whatsapp-mcp/issues/59)
* **bridge:** forward reaction webhook events ([#129](https://github.com/Tauri-EPO/whatsapp-mcp/issues/129)) ([2c8062f](https://github.com/Tauri-EPO/whatsapp-mcp/commit/2c8062fc1990580fe2172b0d88bc1db30d0100cb))
* **bridge:** media auto-download switch, retention sweep, store size in health ([#91](https://github.com/Tauri-EPO/whatsapp-mcp/issues/91)) ([cf19f6b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/cf19f6bc94eba8c0bbac6a8d28d7240ed01f302f)), closes [#60](https://github.com/Tauri-EPO/whatsapp-mcp/issues/60)
* **bridge:** persist chat read state from read receipts + history-sync backfill ([#155](https://github.com/Tauri-EPO/whatsapp-mcp/issues/155)) ([6c6ec00](https://github.com/Tauri-EPO/whatsapp-mcp/commit/6c6ec006a1e67d236172425dbb96a74d6cb9c7c3))
* **bridge:** recover expired media via WhatsApp media-retry ([#21](https://github.com/Tauri-EPO/whatsapp-mcp/issues/21)) ([e9a307c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/e9a307c5a7d876c76f9093f54f84fa7e1ee85ca7)), closes [#6](https://github.com/Tauri-EPO/whatsapp-mcp/issues/6)
* **bridge:** refuse to start when another bridge holds the same store ([#24](https://github.com/Tauri-EPO/whatsapp-mcp/issues/24)) ([3f27cd1](https://github.com/Tauri-EPO/whatsapp-mcp/commit/3f27cd18282de29ae47d3dccd53f884b660dfce3)), closes [#11](https://github.com/Tauri-EPO/whatsapp-mcp/issues/11)
* **bridge:** REST starts before pairing; /api/health is liveness, /api/ready readiness ([#85](https://github.com/Tauri-EPO/whatsapp-mcp/issues/85)) ([5a2e090](https://github.com/Tauri-EPO/whatsapp-mcp/commit/5a2e090319efc1dd4379866456c10f24ffcea34a)), closes [#55](https://github.com/Tauri-EPO/whatsapp-mcp/issues/55)
* **bridge:** WHATSAPP_BRIDGE_BIND and WHATSAPP_BRIDGE_ALLOWED_HOSTS ([#90](https://github.com/Tauri-EPO/whatsapp-mcp/issues/90)) ([716a87a](https://github.com/Tauri-EPO/whatsapp-mcp/commit/716a87a0fe73189008e9ad5ec064cecdc9b5adbd)), closes [#58](https://github.com/Tauri-EPO/whatsapp-mcp/issues/58)
* capture inbound reactions and add /api/react endpoint ([#108](https://github.com/Tauri-EPO/whatsapp-mcp/issues/108)) ([58645d5](https://github.com/Tauri-EPO/whatsapp-mcp/commit/58645d53c57d619dd22454e4b52ab0c41538f445)), closes [#106](https://github.com/Tauri-EPO/whatsapp-mcp/issues/106)
* conversation allow-list (WHATSAPP_ALLOWED_CHATS) for least-privilege agents ([#39](https://github.com/Tauri-EPO/whatsapp-mcp/issues/39)) ([043dbfd](https://github.com/Tauri-EPO/whatsapp-mcp/commit/043dbfdc9074a49d50c7980a6370b1f571fd187f)), closes [#15](https://github.com/Tauri-EPO/whatsapp-mcp/issues/15)
* delete_message tool (revoke for everyone / local delete) + /api/delete ([#41](https://github.com/Tauri-EPO/whatsapp-mcp/issues/41)) ([69b0bde](https://github.com/Tauri-EPO/whatsapp-mcp/commit/69b0bdef6caa79bf76b4e72e2783b05f7b24bb18)), closes [#16](https://github.com/Tauri-EPO/whatsapp-mcp/issues/16)
* Docker Compose stack for bridge + MCP over streamable HTTP ([#22](https://github.com/Tauri-EPO/whatsapp-mcp/issues/22)) ([a340663](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a3406638f56a466f11e7eab3e9ece870e3ecb448)), closes [#7](https://github.com/Tauri-EPO/whatsapp-mcp/issues/7)
* edit and forward messages ([#162](https://github.com/Tauri-EPO/whatsapp-mcp/issues/162)) ([99d2115](https://github.com/Tauri-EPO/whatsapp-mcp/commit/99d2115715eb0b3be6fe60bdc48af6d0b77e617e)), closes [#121](https://github.com/Tauri-EPO/whatsapp-mcp/issues/121)
* FTS5 full-text message search (accent-insensitive, whole words, operators) ([#38](https://github.com/Tauri-EPO/whatsapp-mcp/issues/38)) ([495588e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/495588e4c8444a649dfe971a888b1781c2bbde2e)), closes [#10](https://github.com/Tauri-EPO/whatsapp-mcp/issues/10)
* group management endpoints and tools, send_typing tool ([#161](https://github.com/Tauri-EPO/whatsapp-mcp/issues/161)) ([4c835b8](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4c835b84b1cf3f35838f362693e2ebc6b4bdc8d6)), closes [#120](https://github.com/Tauri-EPO/whatsapp-mcp/issues/120)
* handle incoming media (images, audio, videos, documents) and show names in group messages ([#34](https://github.com/Tauri-EPO/whatsapp-mcp/issues/34)) ([141b9a1](https://github.com/Tauri-EPO/whatsapp-mcp/commit/141b9a156e61f2ddd82380208dc3fca7455221c6))
* list_group_members tool + /api/group/members endpoint ([#40](https://github.com/Tauri-EPO/whatsapp-mcp/issues/40)) ([af4a299](https://github.com/Tauri-EPO/whatsapp-mcp/commit/af4a299bde46ad49620ee9849b53ef9f220070a4)), closes [#17](https://github.com/Tauri-EPO/whatsapp-mcp/issues/17)
* **mcp:** add @-mention support to send_message ([#190](https://github.com/Tauri-EPO/whatsapp-mcp/issues/190)) ([b86a57d](https://github.com/Tauri-EPO/whatsapp-mcp/commit/b86a57d3711f96ba3750d6a4eabf96f8279473b0))
* **mcp:** agent notes on media in notes.db (annotate/get/search_media_notes) ([#185](https://github.com/Tauri-EPO/whatsapp-mcp/issues/185)) ([7e8fc13](https://github.com/Tauri-EPO/whatsapp-mcp/commit/7e8fc138d46bdd523396a8449490233446d0e211)), closes [#97](https://github.com/Tauri-EPO/whatsapp-mcp/issues/97)
* **mcp:** bearer-token auth for the http/sse transports (WHATSAPP_MCP_TOKEN) ([#33](https://github.com/Tauri-EPO/whatsapp-mcp/issues/33)) ([d869efb](https://github.com/Tauri-EPO/whatsapp-mcp/commit/d869efb2d5c71c53fd5c15fd98871738c04c43c6)), closes [#26](https://github.com/Tauri-EPO/whatsapp-mcp/issues/26)
* **mcp:** bridge_status tool ([#159](https://github.com/Tauri-EPO/whatsapp-mcp/issues/159)) ([69f65f0](https://github.com/Tauri-EPO/whatsapp-mcp/commit/69f65f03f14239e1543a202ca48e0c29a1ebe72e)), closes [#118](https://github.com/Tauri-EPO/whatsapp-mcp/issues/118)
* **mcp:** cursor pagination with has_more on the list tools ([#157](https://github.com/Tauri-EPO/whatsapp-mcp/issues/157)) ([0ed507a](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0ed507a8a25fb04f05c930f609d1c6ec677971e7)), closes [#110](https://github.com/Tauri-EPO/whatsapp-mcp/issues/110)
* **mcp:** expose deleted_at on revoked messages, optional include_deleted filter ([#69](https://github.com/Tauri-EPO/whatsapp-mcp/issues/69)) ([1b4fef9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1b4fef90ccfe81b499fcec38d451738328caaab9)), closes [#44](https://github.com/Tauri-EPO/whatsapp-mcp/issues/44)
* **mcp:** expose filename on list_messages / get_message_context ([#23](https://github.com/Tauri-EPO/whatsapp-mcp/issues/23)) ([a1fb223](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a1fb2231f299e59d33c792626411d63d7231fa37)), closes [#9](https://github.com/Tauri-EPO/whatsapp-mcp/issues/9)
* **mcp:** list_unread — unread inbound messages across chats in one call ([#160](https://github.com/Tauri-EPO/whatsapp-mcp/issues/160)) ([8aaf9a0](https://github.com/Tauri-EPO/whatsapp-mcp/commit/8aaf9a06e07d8c21aba7880302af350ad40aedb2)), closes [#119](https://github.com/Tauri-EPO/whatsapp-mcp/issues/119)
* **mcp:** media inventory (list_media, get_media_stats, bytes/sha256 on rows) ([#184](https://github.com/Tauri-EPO/whatsapp-mcp/issues/184)) ([a49f864](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a49f86497a2d228904381aa1a48ef75bfeb00365)), closes [#96](https://github.com/Tauri-EPO/whatsapp-mcp/issues/96)
* **mcp:** migrate to MCP Python SDK v2 and refresh dependencies ([#31](https://github.com/Tauri-EPO/whatsapp-mcp/issues/31)) ([da4774e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/da4774e07bfbc6f27529305bd4bc71ee21acc44a))
* **mcp:** one error envelope for every tool ([#156](https://github.com/Tauri-EPO/whatsapp-mcp/issues/156)) ([d4b76d9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/d4b76d9dac87e253e4a3a36f1fe2409d2c259347)), closes [#116](https://github.com/Tauri-EPO/whatsapp-mcp/issues/116)
* **mcp:** one name for the chat argument, one argument order ([#155](https://github.com/Tauri-EPO/whatsapp-mcp/issues/155)) ([875d7bb](https://github.com/Tauri-EPO/whatsapp-mcp/commit/875d7bbf6d9da2d93cef729ae4e09f81cb1d5f46)), closes [#115](https://github.com/Tauri-EPO/whatsapp-mcp/issues/115)
* **mcp:** optional bearer token on /metrics (WHATSAPP_MCP_METRICS_TOKEN) ([#196](https://github.com/Tauri-EPO/whatsapp-mcp/issues/196)) ([e692b79](https://github.com/Tauri-EPO/whatsapp-mcp/commit/e692b79accecfb2ed3c8ca7ab4aec3d6d29c6844)), closes [#190](https://github.com/Tauri-EPO/whatsapp-mcp/issues/190)
* **mcp:** per-client rate limit and request body cap on the HTTP transport ([#79](https://github.com/Tauri-EPO/whatsapp-mcp/issues/79)) ([009e912](https://github.com/Tauri-EPO/whatsapp-mcp/commit/009e9127165fb647b3344627ed4b091f6848f2ef)), closes [#61](https://github.com/Tauri-EPO/whatsapp-mcp/issues/61)
* **mcp:** reuse the bridge token for HTTP auth when WHATSAPP_MCP_TOKEN is unset ([#78](https://github.com/Tauri-EPO/whatsapp-mcp/issues/78)) ([7662dad](https://github.com/Tauri-EPO/whatsapp-mcp/commit/7662dad6583928bfb20d615b158edbde20d424ea)), closes [#57](https://github.com/Tauri-EPO/whatsapp-mcp/issues/57)
* **mcp:** support http and sse transports via env var ([#112](https://github.com/Tauri-EPO/whatsapp-mcp/issues/112)) ([f885c6c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/f885c6c37a8512f6df7437c0779b32e10fb1823c))
* **mcp:** transcribe_audio tool with local whisper.cpp + compose profile ([#25](https://github.com/Tauri-EPO/whatsapp-mcp/issues/25)) ([953c945](https://github.com/Tauri-EPO/whatsapp-mcp/commit/953c945a67afe29acbf2200fc82549e75f946353)), closes [#8](https://github.com/Tauri-EPO/whatsapp-mcp/issues/8)
* **mcp:** unread_only filter on list_messages ([#84](https://github.com/Tauri-EPO/whatsapp-mcp/issues/84)) ([2b5278f](https://github.com/Tauri-EPO/whatsapp-mcp/commit/2b5278f3d3c12cc6500af8693ebe827012f74262)), closes [#70](https://github.com/Tauri-EPO/whatsapp-mcp/issues/70)
* native WhatsApp polls (creation, votes, tally) + get_poll_results tool ([#42](https://github.com/Tauri-EPO/whatsapp-mcp/issues/42)) ([c962968](https://github.com/Tauri-EPO/whatsapp-mcp/commit/c962968aa78e135bdedc81c81585223ac6ba34f5)), closes [#19](https://github.com/Tauri-EPO/whatsapp-mcp/issues/19)
* on-demand media purge (POST /api/media/purge + purge_media tool, dry run default) ([#186](https://github.com/Tauri-EPO/whatsapp-mcp/issues/186)) ([dfe5602](https://github.com/Tauri-EPO/whatsapp-mcp/commit/dfe56020abdd29e1446b4037139d56bf586f81c4)), closes [#98](https://github.com/Tauri-EPO/whatsapp-mcp/issues/98)
* **ops:** scripts/smoke.sh post-deploy check, run against the compose stack in CI ([#195](https://github.com/Tauri-EPO/whatsapp-mcp/issues/195)) ([eaa5cf9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/eaa5cf9ddf98937fedd10ecc346caf1037807e76))
* optional caption on send_file ([#193](https://github.com/Tauri-EPO/whatsapp-mcp/issues/193)) ([9f6324c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9f6324c92f6a30e3231d8337871e206014a2b66a))
* persist inbound quoted_message_id and add reply support to /api/send ([#109](https://github.com/Tauri-EPO/whatsapp-mcp/issues/109)) ([731e3eb](https://github.com/Tauri-EPO/whatsapp-mcp/commit/731e3eb46ab94200396d92607c6b8e8f27c306d9)), closes [#107](https://github.com/Tauri-EPO/whatsapp-mcp/issues/107)
* send files and audio messages ([9a9fc25](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9a9fc2526da3f5fd5b663423ab8b80daa0b2e7e8))
* send tools return the sent message's id, chat and timestamp ([#158](https://github.com/Tauri-EPO/whatsapp-mcp/issues/158)) ([9c24c86](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9c24c8666c6b9241aee2d2f4bf3212f793fff12f)), closes [#117](https://github.com/Tauri-EPO/whatsapp-mcp/issues/117)
* structured JSON logs and /metrics on both processes ([#178](https://github.com/Tauri-EPO/whatsapp-mcp/issues/178)) ([a110cfc](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a110cfcb830c12704ed3fe2c2f2431d12d0854ac)), closes [#133](https://github.com/Tauri-EPO/whatsapp-mcp/issues/133)
* WHATSAPP_STORE_DIR — store location no longer depends on the working directory ([#83](https://github.com/Tauri-EPO/whatsapp-mcp/issues/83)) ([21bd0a2](https://github.com/Tauri-EPO/whatsapp-mcp/commit/21bd0a2864f4b56a13b93e73a4f037ef19912f15)), closes [#51](https://github.com/Tauri-EPO/whatsapp-mcp/issues/51)


### Bug fixes

* **bridge:** archive view-once media instead of dropping it ([#77](https://github.com/Tauri-EPO/whatsapp-mcp/issues/77)) ([2e0bbac](https://github.com/Tauri-EPO/whatsapp-mcp/commit/2e0bbacf1a07d42a04aa821a9a73f580f8572a10)), closes [#67](https://github.com/Tauri-EPO/whatsapp-mcp/issues/67)
* **bridge:** attribute history-sync group messages to the participant ([#36](https://github.com/Tauri-EPO/whatsapp-mcp/issues/36)) ([f7ae835](https://github.com/Tauri-EPO/whatsapp-mcp/commit/f7ae83541029c2dba324547cc353f6b81c8d372b)), closes [#12](https://github.com/Tauri-EPO/whatsapp-mcp/issues/12)
* **bridge:** authenticate outbound webhook POSTs ([3e8b919](https://github.com/Tauri-EPO/whatsapp-mcp/commit/3e8b919290310fbaedfe9bcfa29f03f8336cd1e2))
* **bridge:** auto-download runs after StoreMessage to avoid lookup race ([#41](https://github.com/Tauri-EPO/whatsapp-mcp/issues/41)) ([6a711b7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/6a711b72740b44715c0c7e94ac4c432ee533ff32))
* **bridge:** bump whatsmeow for client compatibility ([#182](https://github.com/Tauri-EPO/whatsapp-mcp/issues/182)) ([544e9e7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/544e9e737e980cf652794262fb0f3957e5c10fa9))
* **bridge:** bump whatsmeow so WhatsApp accepts new device pairing ([#128](https://github.com/Tauri-EPO/whatsapp-mcp/issues/128)) ([c5cc5c7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/c5cc5c78dcdc64ded482bf451507a785f5afbed6))
* **bridge:** cached documents keep the sender's extension; legacy names still served ([#200](https://github.com/Tauri-EPO/whatsapp-mcp/issues/200)) ([1f4a7b1](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1f4a7b12f5528a111325cda02c84c951d3012119)), closes [#187](https://github.com/Tauri-EPO/whatsapp-mcp/issues/187)
* **bridge:** document filename leaked the sender's absolute path ([#1](https://github.com/Tauri-EPO/whatsapp-mcp/issues/1)) ([4456832](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4456832b6a4fc2f53e3a354a5f87f10bbdf43527))
* **bridge:** every SQLite handle uses WAL and a busy timeout ([#167](https://github.com/Tauri-EPO/whatsapp-mcp/issues/167)) ([9119569](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9119569e00c02bda2d2731c158119f6a6fbbd02b))
* **bridge:** exit on LoggedOut / ClientOutdated so the supervisor re-pairs ([#141](https://github.com/Tauri-EPO/whatsapp-mcp/issues/141)) ([626ad9b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/626ad9b62582cf406c98daec9dcb0fccffa98917)), closes [#100](https://github.com/Tauri-EPO/whatsapp-mcp/issues/100)
* **bridge:** extract text from Template, Button, Interactive, and List messages ([#134](https://github.com/Tauri-EPO/whatsapp-mcp/issues/134)) ([a76645a](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a76645ae8ca5a80e3e388fd62751c45a3a23933d))
* **bridge:** fall back to local whatsmeow_contacts for chat name resolution ([#135](https://github.com/Tauri-EPO/whatsapp-mcp/issues/135)) ([839d069](https://github.com/Tauri-EPO/whatsapp-mcp/commit/839d069ec95902a433882cf75cc5d26e3710686e))
* **bridge:** forward native WhatsApp activation metadata ([#173](https://github.com/Tauri-EPO/whatsapp-mcp/issues/173)) ([b823a9e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/b823a9e282356c734a9ae3d36291a76a46102603))
* **bridge:** handle ProtocolMessage_REVOKE (delete-for-everyone) events ([#99](https://github.com/Tauri-EPO/whatsapp-mcp/issues/99)) ([ab08698](https://github.com/Tauri-EPO/whatsapp-mcp/commit/ab08698eb56f19d6b360b5c452fcab280d3d33cd))
* **bridge:** handle StreamReplaced event to recover from session conflicts ([#27](https://github.com/Tauri-EPO/whatsapp-mcp/issues/27)) ([27a4b5c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/27a4b5c42a85438ab806cf27ab5032f830fdb433))
* **bridge:** include message ID in media filenames to prevent same-second collisions ([#40](https://github.com/Tauri-EPO/whatsapp-mcp/issues/40)) ([02ce549](https://github.com/Tauri-EPO/whatsapp-mcp/commit/02ce5494f72e0ada65d6e21e2f7990aa6170d0c5))
* **bridge:** keep CDN auth tokens in directPath to fix 403 media downloads ([#132](https://github.com/Tauri-EPO/whatsapp-mcp/issues/132)) ([4e354af](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4e354afde7321dbd4875ed1a870539e05ef1443d))
* **bridge:** log send caller identity ([#96](https://github.com/Tauri-EPO/whatsapp-mcp/issues/96)) ([1979b73](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1979b732d4c6a8ed77e44a91d36a812ff3b4c311))
* **bridge:** normalise quoted-reply participant JID (bare numbers, LID groups) ([#37](https://github.com/Tauri-EPO/whatsapp-mcp/issues/37)) ([16cadf9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/16cadf9112297322e645161d809ac516550798d8)), closes [#13](https://github.com/Tauri-EPO/whatsapp-mcp/issues/13)
* **bridge:** ordered shutdown ([#146](https://github.com/Tauri-EPO/whatsapp-mcp/issues/146)) ([83de29d](https://github.com/Tauri-EPO/whatsapp-mcp/commit/83de29d7081ec6f28bec2e52b58a3df4bee50d3d)), closes [#105](https://github.com/Tauri-EPO/whatsapp-mcp/issues/105)
* **bridge:** pass message store when sending messages ([#91](https://github.com/Tauri-EPO/whatsapp-mcp/issues/91)) ([c86b1b2](https://github.com/Tauri-EPO/whatsapp-mcp/commit/c86b1b210bb1902a683a9e0c443dbe01316c29b7))
* **bridge:** persist shared contact cards (vCards) as searchable text ([#3](https://github.com/Tauri-EPO/whatsapp-mcp/issues/3)) ([28cf259](https://github.com/Tauri-EPO/whatsapp-mcp/commit/28cf25956d92358f7c3ddbdf2af479253137e3d0))
* **bridge:** preserve message ID in text webhooks ([#2](https://github.com/Tauri-EPO/whatsapp-mcp/issues/2)) ([da8b323](https://github.com/Tauri-EPO/whatsapp-mcp/commit/da8b323d428a6ccf2d628d14f62128bb73a6909d))
* **bridge:** preserve original timestamp on retry-redelivered messages ([#149](https://github.com/Tauri-EPO/whatsapp-mcp/issues/149)) ([45e674e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/45e674ef0667303a690bc82de0d8bbc61f4dfd40))
* **bridge:** QR pairing retries immediately on a timeout event ([#147](https://github.com/Tauri-EPO/whatsapp-mcp/issues/147)) ([0bacb06](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0bacb06cac67dcce868844549e628a627314f5ea)), closes [#106](https://github.com/Tauri-EPO/whatsapp-mcp/issues/106)
* **bridge:** redraw the pairing QR code on every whatsmeow rotation ([#34](https://github.com/Tauri-EPO/whatsapp-mcp/issues/34)) ([2f31fd0](https://github.com/Tauri-EPO/whatsapp-mcp/commit/2f31fd0f2ffa761bd9f853d2d6a9eb13dc8771cf)), closes [#14](https://github.com/Tauri-EPO/whatsapp-mcp/issues/14)
* **bridge:** resolve [@lid](https://github.com/lid) sender to phone JID in webhook payload ([#56](https://github.com/Tauri-EPO/whatsapp-mcp/issues/56)) ([0a36db4](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0a36db4c70a2ed423ffe75e764e0d33092b67717))
* **bridge:** set FileName and detect MIME for document sends ([#95](https://github.com/Tauri-EPO/whatsapp-mcp/issues/95)) ([28aa25c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/28aa25c77d3ba5e97dc65c47216f57b39e926414))
* **bridge:** surface image/video/document captions in extractTextContent ([#42](https://github.com/Tauri-EPO/whatsapp-mcp/issues/42)) ([33ef0c0](https://github.com/Tauri-EPO/whatsapp-mcp/commit/33ef0c057ad47b364f50eba35b074d04a0bd64e1))
* **ci:** make dependabot auto-approve non-fatal ([#65](https://github.com/Tauri-EPO/whatsapp-mcp/issues/65)) ([b671e82](https://github.com/Tauri-EPO/whatsapp-mcp/commit/b671e827d2f728f24b5037b525c27694dc6b428f))
* **compose:** recreate mcp/whisper whenever the bridge is recreated ([#143](https://github.com/Tauri-EPO/whatsapp-mcp/issues/143)) ([7b909b8](https://github.com/Tauri-EPO/whatsapp-mcp/commit/7b909b89cc2a57fee5bec02846d827da803b4de5)), closes [#102](https://github.com/Tauri-EPO/whatsapp-mcp/issues/102)
* **compose:** start mcp/whisper only after the bridge is healthy ([#95](https://github.com/Tauri-EPO/whatsapp-mcp/issues/95)) ([0e1cb9c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0e1cb9ccfe4c052e4a3d1ae9062867ce880fadb7))
* **deps:** preserve Intel macOS cryptography installs ([#188](https://github.com/Tauri-EPO/whatsapp-mcp/issues/188)) ([cc43c7b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/cc43c7b02d0c385cbebedac84dbb4b40103e1fa2))
* enable stricter Go linting (errcheck, unused, ineffassign) ([dc1e7fa](https://github.com/Tauri-EPO/whatsapp-mcp/commit/dc1e7fa05bf3d879d63177b79c4f9b0d1209dc51))
* **env:** ship .env.example with webhooks off ([#142](https://github.com/Tauri-EPO/whatsapp-mcp/issues/142)) ([44c75e9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/44c75e9ab8d1201ddf77ce93bcc10ad44fcd028e)), closes [#101](https://github.com/Tauri-EPO/whatsapp-mcp/issues/101)
* escape monitor-script shell vars in launchd installer ([#130](https://github.com/Tauri-EPO/whatsapp-mcp/issues/130)) ([a3fcaf9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a3fcaf94fa31a5824a4b16677586fc71a6556935))
* exit orphaned stdio MCP servers on parent death ([#177](https://github.com/Tauri-EPO/whatsapp-mcp/issues/177)) ([a6869f4](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a6869f4fb4c2549efa7317872f1dfa7a553dc67a))
* harden read-state observe/act after [#155](https://github.com/Tauri-EPO/whatsapp-mcp/issues/155) and [#201](https://github.com/Tauri-EPO/whatsapp-mcp/issues/201) ([#203](https://github.com/Tauri-EPO/whatsapp-mcp/issues/203)) ([c3fbd45](https://github.com/Tauri-EPO/whatsapp-mcp/commit/c3fbd45d6aec4f3a7668f814634910e4ddd3fcd4))
* **mcp:** cap list_messages context windows and total rows ([#151](https://github.com/Tauri-EPO/whatsapp-mcp/issues/151)) ([252c3f7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/252c3f75d6327db23f8f80115777908eb858b96b))
* **mcp:** deduplicate contact chat results ([0da7399](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0da7399b5ad04b1972a16ceae7abd5fcab4a63a3))
* **mcp:** ffmpeg conversions run with a timeout ([#145](https://github.com/Tauri-EPO/whatsapp-mcp/issues/145)) ([8b8a0dc](https://github.com/Tauri-EPO/whatsapp-mcp/commit/8b8a0dc6f852db992bdcede51d8cc5b8dde9e4b2)), closes [#104](https://github.com/Tauri-EPO/whatsapp-mcp/issues/104)
* **mcp:** honour WHATSAPP_MCP_HOST for DNS-rebinding allow-list (421 on non-loopback Host) ([#20](https://github.com/Tauri-EPO/whatsapp-mcp/issues/20)) ([0716add](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0716addca3b535bcbba7d6285dd39f9dcb8aba5b))
* **mcp:** log diagnostics to stderr instead of print() (stdio-safe) ([#66](https://github.com/Tauri-EPO/whatsapp-mcp/issues/66)) ([2a5ab5b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/2a5ab5bebaa0fa6a0b49c88413f77190b3f46eb2)), closes [#43](https://github.com/Tauri-EPO/whatsapp-mcp/issues/43)
* **mcp:** match messages by both phone number and LID via whatsmeow_lid_map ([#43](https://github.com/Tauri-EPO/whatsapp-mcp/issues/43)) ([7c4f129](https://github.com/Tauri-EPO/whatsapp-mcp/commit/7c4f1296e52b065ea6917d2800c081fe3f4debe4))
* **mcp:** resolve bare numeric LIDs ([#97](https://github.com/Tauri-EPO/whatsapp-mcp/issues/97)) ([a8779ab](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a8779abdccece147cef832166c684086043e808a))
* **mcp:** resolve bridge token from whatsmeow db path ([5926173](https://github.com/Tauri-EPO/whatsapp-mcp/commit/592617391fdfc67f1a44a5471cfed5bcad2cb43f)), closes [#142](https://github.com/Tauri-EPO/whatsapp-mcp/issues/142)
* **mcp:** resolve contacts via whatsmeow store with LID → phone fallback ([#30](https://github.com/Tauri-EPO/whatsapp-mcp/issues/30)) ([0c846a4](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0c846a40ecc8ef3408693a9c0180491e173ba10d))
* **mcp:** serialize message context result ([af0aed6](https://github.com/Tauri-EPO/whatsapp-mcp/commit/af0aed6d3a8817ec6f20de34722b5dc482a113f1))
* **mcp:** timeout and connection retry on every bridge REST call ([#144](https://github.com/Tauri-EPO/whatsapp-mcp/issues/144)) ([7489c06](https://github.com/Tauri-EPO/whatsapp-mcp/commit/7489c068dc571417188eec9a0d7b076737c4685b)), closes [#103](https://github.com/Tauri-EPO/whatsapp-mcp/issues/103)
* migrate legacy [@lid](https://github.com/lid) chat rows to phone JIDs ([#13](https://github.com/Tauri-EPO/whatsapp-mcp/issues/13)) ([e838d31](https://github.com/Tauri-EPO/whatsapp-mcp/commit/e838d31fdd7321479a2413206cf6e5f339c67a66))
* pin anyio&lt;4.9 to avoid cancel scope regression ([#44](https://github.com/Tauri-EPO/whatsapp-mcp/issues/44)) ([21a85b7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/21a85b776218b1872510f65aecc1766152ccfef6))
* read WHATSAPP_BRIDGE_PORT env var for REST API port ([#9](https://github.com/Tauri-EPO/whatsapp-mcp/issues/9)) ([1ae13c5](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1ae13c5cfd9d6dc0b7393fa493eb092162579001))
* remove version property for golangci-lint v1.x compatibility ([c4fd9c1](https://github.com/Tauri-EPO/whatsapp-mcp/commit/c4fd9c1dfcc5897b1a9856884e507ce6de56e4fc))
* resolve LID contacts in get_contact ([#7](https://github.com/Tauri-EPO/whatsapp-mcp/issues/7)) ([a1503e6](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a1503e64ceb61ee15cfb1e8a52d9be7561fa79a7))
* resolve LID JIDs to phone-based JIDs for consistent chat storage ([#12](https://github.com/Tauri-EPO/whatsapp-mcp/issues/12)) ([466a403](https://github.com/Tauri-EPO/whatsapp-mcp/commit/466a4032e449cd15d82ae8691079f128a3781525))
* resolve phone number JIDs to LIDs before sending ([#8](https://github.com/Tauri-EPO/whatsapp-mcp/issues/8)) ([6cdfe2a](https://github.com/Tauri-EPO/whatsapp-mcp/commit/6cdfe2a8069b84cb7b6414c5817f24d857c3a957))
* security hardening for LAN exposure and Unicode search ([#55](https://github.com/Tauri-EPO/whatsapp-mcp/issues/55)) ([05e639b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/05e639ba8560c0a1475a45f9ae5ecd928058e4fa))
* send and track disappearing-message settings correctly ([#82](https://github.com/Tauri-EPO/whatsapp-mcp/issues/82)) ([e07bd23](https://github.com/Tauri-EPO/whatsapp-mcp/commit/e07bd2320d8466d02d42cafdf5aaed859948ff8e))
* **server:** list_chats/get_chat error when include_last_message=False ([#79](https://github.com/Tauri-EPO/whatsapp-mcp/issues/79)) ([6cfdb2b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/6cfdb2bda891dd7de3f359effded7b575fbaaf79))


### Performance

* **bridge:** cache chat names, no group-info fetch during history sync, drop reflection ([#72](https://github.com/Tauri-EPO/whatsapp-mcp/issues/72)) ([d7acd0f](https://github.com/Tauri-EPO/whatsapp-mcp/commit/d7acd0fe3c3ab49265418fd41fecf60c6e16dcf8)), closes [#46](https://github.com/Tauri-EPO/whatsapp-mcp/issues/46)
* **bridge:** chat name cache with TTL, pruning and group-rename updates ([#153](https://github.com/Tauri-EPO/whatsapp-mcp/issues/153)) ([7fdd004](https://github.com/Tauri-EPO/whatsapp-mcp/commit/7fdd0044ba1bb39e1135d361873f3d11d41dee1a)), closes [#114](https://github.com/Tauri-EPO/whatsapp-mcp/issues/114)
* **bridge:** history sync writes one transaction per conversation ([#150](https://github.com/Tauri-EPO/whatsapp-mcp/issues/150)) ([6f7c2a9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/6f7c2a9dc6fad3cd66b96a665c03701117d3caf5)), closes [#109](https://github.com/Tauri-EPO/whatsapp-mcp/issues/109)
* **bridge:** index messages(chat_jid) to speed up LID migration ([#144](https://github.com/Tauri-EPO/whatsapp-mcp/issues/144)) ([609ddc2](https://github.com/Tauri-EPO/whatsapp-mcp/commit/609ddc2056406dc40d226c4fd4ecded92693dc08))
* **bridge:** stream media downloads to disk; cap auto-download size ([#154](https://github.com/Tauri-EPO/whatsapp-mcp/issues/154)) ([9b3c5a2](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9b3c5a24082424043bd1cbd50dbe904924ebf047)), closes [#112](https://github.com/Tauri-EPO/whatsapp-mcp/issues/112)
* **mcp:** cache sender names, exact-match lookup, one whatsapp.db handle ([#149](https://github.com/Tauri-EPO/whatsapp-mcp/issues/149)) ([5eba2bd](https://github.com/Tauri-EPO/whatsapp-mcp/commit/5eba2bd735cda15ad8d0772a6adde39638a5be28)), closes [#107](https://github.com/Tauri-EPO/whatsapp-mcp/issues/107)
* **mcp:** fetch list_messages context in one query, dedupe per chat ([#75](https://github.com/Tauri-EPO/whatsapp-mcp/issues/75)) ([edfd73c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/edfd73cfc2dbe6185d095c6346a8804c9a9b4cb6)), closes [#45](https://github.com/Tauri-EPO/whatsapp-mcp/issues/45)
* **mcp:** memoise schema probes; exact direct-chat lookup ([#152](https://github.com/Tauri-EPO/whatsapp-mcp/issues/152)) ([07c4651](https://github.com/Tauri-EPO/whatsapp-mcp/commit/07c46519f6c4207b896451e6342e9b05f0b1b93e)), closes [#113](https://github.com/Tauri-EPO/whatsapp-mcp/issues/113)
* SQLite WAL + busy timeout, indexed (id, chat_jid) message-context lookup ([#35](https://github.com/Tauri-EPO/whatsapp-mcp/issues/35)) ([a8f54f4](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a8f54f4b584770f957bf5ecd62764d90d00e98d0)), closes [#18](https://github.com/Tauri-EPO/whatsapp-mcp/issues/18)
* **store:** index messages.file_sha256 for media inventory and notes ([#194](https://github.com/Tauri-EPO/whatsapp-mcp/issues/194)) ([d866868](https://github.com/Tauri-EPO/whatsapp-mcp/commit/d86686867539334b1a0591170b76108b5b58a859)), closes [#188](https://github.com/Tauri-EPO/whatsapp-mcp/issues/188)
* **store:** indexes for the queries the MCP server runs ([#148](https://github.com/Tauri-EPO/whatsapp-mcp/issues/148)) ([0577e85](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0577e857d42ad8649e45816cfad09455ece75687)), closes [#108](https://github.com/Tauri-EPO/whatsapp-mcp/issues/108)


### Refactoring

* **bridge:** Bridge struct replaces policy/decrypter/downloader/forward-self globals ([#80](https://github.com/Tauri-EPO/whatsapp-mcp/issues/80)) ([f29189e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/f29189efa7c0e90571c2f8784bf6fc4995e3e995)), closes [#47](https://github.com/Tauri-EPO/whatsapp-mcp/issues/47)
* **bridge:** handlers pass the request context to WhatsApp calls ([#166](https://github.com/Tauri-EPO/whatsapp-mcp/issues/166)) ([7366695](https://github.com/Tauri-EPO/whatsapp-mcp/commit/7366695cca840751e1412d41c8f0b7af9e39af68)), closes [#125](https://github.com/Tauri-EPO/whatsapp-mcp/issues/125)
* **bridge:** move webhook sender, timestamp registry and media-retry hub onto Bridge ([#81](https://github.com/Tauri-EPO/whatsapp-mcp/issues/81)) ([f0695cd](https://github.com/Tauri-EPO/whatsapp-mcp/commit/f0695cd292884088a646b8416cbd92dc3fd1e723)), closes [#47](https://github.com/Tauri-EPO/whatsapp-mcp/issues/47)
* **bridge:** one extractMessage/persistMessage for live and history paths ([#163](https://github.com/Tauri-EPO/whatsapp-mcp/issues/163)) ([865802b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/865802b98de2a2e37c12a353e2159fe9ddaa97c6)), closes [#122](https://github.com/Tauri-EPO/whatsapp-mcp/issues/122)
* **bridge:** resolve configuration once; Bridge fields on the hot paths ([#165](https://github.com/Tauri-EPO/whatsapp-mcp/issues/165)) ([5cde26c](https://github.com/Tauri-EPO/whatsapp-mcp/commit/5cde26cf0fbbae1fb6bf18af2dc17ee2c74249fd)), closes [#124](https://github.com/Tauri-EPO/whatsapp-mcp/issues/124)
* **bridge:** route all bridge output through one leveled logger ([#89](https://github.com/Tauri-EPO/whatsapp-mcp/issues/89)) ([859cb0b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/859cb0b14e4c40a48e2ec78779e68d32d841a0fc)), closes [#54](https://github.com/Tauri-EPO/whatsapp-mcp/issues/54)
* **bridge:** route every connection check through Bridge.Connected; drop min() ([#197](https://github.com/Tauri-EPO/whatsapp-mcp/issues/197)) ([a5a74e0](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a5a74e0791934ea84a5b10cf776678117eacf94c)), closes [#191](https://github.com/Tauri-EPO/whatsapp-mcp/issues/191)
* **bridge:** share recipient JID resolution ([#90](https://github.com/Tauri-EPO/whatsapp-mcp/issues/90)) ([94e6ee9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/94e6ee99e9773f269800a296a2be11bafff5f88a))
* **bridge:** split main.go into files by responsibility ([#93](https://github.com/Tauri-EPO/whatsapp-mcp/issues/93)) ([3d4fe57](https://github.com/Tauri-EPO/whatsapp-mcp/commit/3d4fe570ad01370c99b4b3d4e2183efe5f760771))
* **bridge:** split newRESTMux, JSON errors everywhere, method middleware, request log ([#164](https://github.com/Tauri-EPO/whatsapp-mcp/issues/164)) ([54247a4](https://github.com/Tauri-EPO/whatsapp-mcp/commit/54247a4ebe37ac3c207d69c0fdbd3d8faec40605)), closes [#123](https://github.com/Tauri-EPO/whatsapp-mcp/issues/123)
* **mcp:** bridge and whisper clients on httpx, drop requests ([#198](https://github.com/Tauri-EPO/whatsapp-mcp/issues/198)) ([cfd1224](https://github.com/Tauri-EPO/whatsapp-mcp/commit/cfd1224eebf989cddd6a73ed6698d6cf1b0bc837)), closes [#192](https://github.com/Tauri-EPO/whatsapp-mcp/issues/192)
* **mcp:** one column list and one row→Message mapper ([#68](https://github.com/Tauri-EPO/whatsapp-mcp/issues/68)) ([07c5e84](https://github.com/Tauri-EPO/whatsapp-mcp/commit/07c5e84b27df4b5df75792a614589deb57963013)), closes [#50](https://github.com/Tauri-EPO/whatsapp-mcp/issues/50)
* target_message_id for reactions and poll votes (stop overloading filename) ([#82](https://github.com/Tauri-EPO/whatsapp-mcp/issues/82)) ([53e0085](https://github.com/Tauri-EPO/whatsapp-mcp/commit/53e0085795b3425746e1b3fc64a876bd060a3d48)), closes [#49](https://github.com/Tauri-EPO/whatsapp-mcp/issues/49)


### Documentation

* add contributors section and updating instructions to README ([#67](https://github.com/Tauri-EPO/whatsapp-mcp/issues/67)) ([f6fa977](https://github.com/Tauri-EPO/whatsapp-mcp/commit/f6fa9775992cdb7c347a101baa8d3eef4d684a7b))
* add ROADMAP, AGENTS, CONTRIBUTING, CODEOWNERS, issue/PR templates ([#47](https://github.com/Tauri-EPO/whatsapp-mcp/issues/47)) ([985f8d8](https://github.com/Tauri-EPO/whatsapp-mcp/commit/985f8d8d22dcbfebda900768aaf8ed9c79d4e6f7))
* add Windows compatibility instructions to README ([118b136](https://github.com/Tauri-EPO/whatsapp-mcp/commit/118b136619a663342aa037201314a696273242c0))
* **agents:** point the routine at epics [#138](https://github.com/Tauri-EPO/whatsapp-mcp/issues/138) and [#99](https://github.com/Tauri-EPO/whatsapp-mcp/issues/99) ([#139](https://github.com/Tauri-EPO/whatsapp-mcp/issues/139)) ([8415097](https://github.com/Tauri-EPO/whatsapp-mcp/commit/84150978eb30aba283fe7c7f36a5d0c7c567482e))
* backup and restore recipe with scripts/backup.sh ([#179](https://github.com/Tauri-EPO/whatsapp-mcp/issues/179)) ([a16faed](https://github.com/Tauri-EPO/whatsapp-mcp/commit/a16faedbc8af1ab719f1a4690a771d7ec5c30db8)), closes [#135](https://github.com/Tauri-EPO/whatsapp-mcp/issues/135)
* describe the fork's goal, differences and principles in README and AGENTS.md ([#32](https://github.com/Tauri-EPO/whatsapp-mcp/issues/32)) ([2eaec59](https://github.com/Tauri-EPO/whatsapp-mcp/commit/2eaec59d775560ae0e0a99ff9ada916af30e1697))
* document WHATSMEOW_DB_PATH env var ([#51](https://github.com/Tauri-EPO/whatsapp-mcp/issues/51)) ([558c392](https://github.com/Tauri-EPO/whatsapp-mcp/commit/558c392ca35126cf3b9e73424e1014c603758bd5))
* hard-fork policy, agent routine and structured AGENTS.md ([#65](https://github.com/Tauri-EPO/whatsapp-mcp/issues/65)) ([3e800a0](https://github.com/Tauri-EPO/whatsapp-mcp/commit/3e800a0447b9c1e63b67615b5d1ac7776f359cfb))
* make AGENTS.md canonical and fix FORWARD_SELF default ([#127](https://github.com/Tauri-EPO/whatsapp-mcp/issues/127)) ([d31fa06](https://github.com/Tauri-EPO/whatsapp-mcp/commit/d31fa060cf9fb4a65b8ce0f25744492c1659d72b))
* README as a landing page; technical reference moves to docs/ ([#140](https://github.com/Tauri-EPO/whatsapp-mcp/issues/140)) ([155eb50](https://github.com/Tauri-EPO/whatsapp-mcp/commit/155eb5024dbf38af06be28b814d701e76e871a29))
* README, AGENTS.md, .env.example, docs/DOCKER.md (Funnel step 1). ([7662dad](https://github.com/Tauri-EPO/whatsapp-mcp/commit/7662dad6583928bfb20d615b158edbde20d424ea))
* **readme:** add demo video overview ([#122](https://github.com/Tauri-EPO/whatsapp-mcp/issues/122)) ([645a7e5](https://github.com/Tauri-EPO/whatsapp-mcp/commit/645a7e5a46c7511f030cd5ef20f658eb8faca844))
* **readme:** document app state recovery ([#117](https://github.com/Tauri-EPO/whatsapp-mcp/issues/117)) ([b2cf46b](https://github.com/Tauri-EPO/whatsapp-mcp/commit/b2cf46bc3bc04b46df7886c8af0bd824b6e3fe9e))
* **readme:** refresh the fork comparison table and feature list ([#94](https://github.com/Tauri-EPO/whatsapp-mcp/issues/94)) ([5519025](https://github.com/Tauri-EPO/whatsapp-mcp/commit/5519025797d5cc4578675f08a644e5769eaae25a))
* update README.md ([4dc00bd](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4dc00bda550db00d92daa9a237809b09c7686071))
* update remaining "go run main.go" references to "go run ." ([#50](https://github.com/Tauri-EPO/whatsapp-mcp/issues/50)) ([de75e53](https://github.com/Tauri-EPO/whatsapp-mcp/commit/de75e53467ba657fb166fda64ad23d8b5cbb9801))
* Update SECURITY.md with enhanced security policy ([#78](https://github.com/Tauri-EPO/whatsapp-mcp/issues/78)) ([8cfaee2](https://github.com/Tauri-EPO/whatsapp-mcp/commit/8cfaee22c4b4371e0634db50a898017a43fabddd))
* update to VGP standards with mermaid diagrams ([c7baf8a](https://github.com/Tauri-EPO/whatsapp-mcp/commit/c7baf8a24329f680777d1ad5aad5f364a7e8feae))
* upstream harvest script and marks ([#74](https://github.com/Tauri-EPO/whatsapp-mcp/issues/74)) ([2e102b9](https://github.com/Tauri-EPO/whatsapp-mcp/commit/2e102b97479d2ea86bb1d2963693f88c97ad4eee)), closes [#62](https://github.com/Tauri-EPO/whatsapp-mcp/issues/62)


### CI and images

* adapt workflows to the fork (no auto-release, no dependabot auto-merge) ([#27](https://github.com/Tauri-EPO/whatsapp-mcp/issues/27)) ([0da7359](https://github.com/Tauri-EPO/whatsapp-mcp/commit/0da73595157fbbab4173ab53c94bfb06dc4c85cb))
* add manual dispatch and fix golangci-lint compatibility ([#11](https://github.com/Tauri-EPO/whatsapp-mcp/issues/11)) ([29bb539](https://github.com/Tauri-EPO/whatsapp-mcp/commit/29bb5395130b010bd8cefed4cd882cec8832761d))
* automatic semantic releases with release-please; vX.Y.Z and latest images ([#210](https://github.com/Tauri-EPO/whatsapp-mcp/issues/210)) ([de3b1e8](https://github.com/Tauri-EPO/whatsapp-mcp/commit/de3b1e871bc4a0c967daffe1030996f84337fe90)), closes [#209](https://github.com/Tauri-EPO/whatsapp-mcp/issues/209)
* build both Docker images and smoke-test them on every PR ([#73](https://github.com/Tauri-EPO/whatsapp-mcp/issues/73)) ([5ea363e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/5ea363ec322d8d63da6ff9a6c4636c3059e4c526)), closes [#52](https://github.com/Tauri-EPO/whatsapp-mcp/issues/52)
* bump Go toolchain to 1.25 across workflows ([#46](https://github.com/Tauri-EPO/whatsapp-mcp/issues/46)) ([d1fc229](https://github.com/Tauri-EPO/whatsapp-mcp/commit/d1fc229eb56d4f2625f7d63124e2f0aab31fe79d))
* cancel superseded runs, one CodeQL run per PR, merged jobs, arm64 build ([#174](https://github.com/Tauri-EPO/whatsapp-mcp/issues/174)) ([9693df0](https://github.com/Tauri-EPO/whatsapp-mcp/commit/9693df06893609086800b29d4a6a726ea787bab8))
* delegate Dependabot auto-merge to org reusable workflow ([#69](https://github.com/Tauri-EPO/whatsapp-mcp/issues/69)) ([640cd72](https://github.com/Tauri-EPO/whatsapp-mcp/commit/640cd7211326eee92e50162876bbdfb5c3248481))
* enable staticcheck, gosec and misspell; fix what they found ([#87](https://github.com/Tauri-EPO/whatsapp-mcp/issues/87)) ([12ff3a7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/12ff3a70772acc7c07dd53d513b067bfc8439c5f))
* **mcp:** pyright in basic mode for the Python server ([#208](https://github.com/Tauri-EPO/whatsapp-mcp/issues/208)) ([1556127](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1556127e7de010d6953db100142c1ded04c329af)), closes [#204](https://github.com/Tauri-EPO/whatsapp-mcp/issues/204)
* modernize linting and add release workflow for first fork release ([#14](https://github.com/Tauri-EPO/whatsapp-mcp/issues/14)) ([e476e68](https://github.com/Tauri-EPO/whatsapp-mcp/commit/e476e682665b0e11d3426124a86ad5501ec18a2b))
* provenance and SBOM on published images, Trivy scan in security.yml ([#207](https://github.com/Tauri-EPO/whatsapp-mcp/issues/207)) ([5ae10e7](https://github.com/Tauri-EPO/whatsapp-mcp/commit/5ae10e7a04206c7256451c0ec0314549faa04c05))
* publish bridge and MCP images to GHCR on push to main ([#199](https://github.com/Tauri-EPO/whatsapp-mcp/issues/199)) ([7e7e5d3](https://github.com/Tauri-EPO/whatsapp-mcp/commit/7e7e5d311def2a69b370e7b509ef9e5e33a05b2f)), closes [#193](https://github.com/Tauri-EPO/whatsapp-mcp/issues/193)
* support merge queue checks ([#174](https://github.com/Tauri-EPO/whatsapp-mcp/issues/174)) ([95081fc](https://github.com/Tauri-EPO/whatsapp-mcp/commit/95081fc6a828563bc347a8a5cd41825ef4ea4a63))


### Tests

* **bridge:** cover content.go extractors and the events.go dispatcher ([#182](https://github.com/Tauri-EPO/whatsapp-mcp/issues/182)) ([dc194fb](https://github.com/Tauri-EPO/whatsapp-mcp/commit/dc194fbb52d4d6b88ba2a7365585b2c653e9a1ba))
* **bridge:** cover downloadMedia and handleDownload ([#181](https://github.com/Tauri-EPO/whatsapp-mcp/issues/181)) ([4cea6c1](https://github.com/Tauri-EPO/whatsapp-mcp/commit/4cea6c1cdf9ed3747910319df0f993c08b703ebe))
* **bridge:** cover send.go builders, Ogg analysis and media classification ([#180](https://github.com/Tauri-EPO/whatsapp-mcp/issues/180)) ([8ce201d](https://github.com/Tauri-EPO/whatsapp-mcp/commit/8ce201dcaf7ad444e05aaa1e8f6c8dceffae31d7))
* **bridge:** media file names agree between arrival and re-download in any TZ ([#205](https://github.com/Tauri-EPO/whatsapp-mcp/issues/205)) ([de4707f](https://github.com/Tauri-EPO/whatsapp-mcp/commit/de4707f4055eeead0b0b9876d4af6d499947da26)), closes [#201](https://github.com/Tauri-EPO/whatsapp-mcp/issues/201)
* **mcp:** cover search_contacts, the LID/name layer, audio.py and transcribe_audio ([#183](https://github.com/Tauri-EPO/whatsapp-mcp/issues/183)) ([8e0454a](https://github.com/Tauri-EPO/whatsapp-mcp/commit/8e0454acbaa20ded09ac4a5e74b670dedafad211)), closes [#137](https://github.com/Tauri-EPO/whatsapp-mcp/issues/137)
* **mcp:** every environment variable is documented in all four places ([#206](https://github.com/Tauri-EPO/whatsapp-mcp/issues/206)) ([1ff7b0e](https://github.com/Tauri-EPO/whatsapp-mcp/commit/1ff7b0e06ebb3550ef493d4df8208f782536d5bd)), closes [#202](https://github.com/Tauri-EPO/whatsapp-mcp/issues/202)
