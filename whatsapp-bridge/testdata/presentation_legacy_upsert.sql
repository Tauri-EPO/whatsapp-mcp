-- Actual bridge upsert from 68ccb96920ab5f8c511ab17464db23cebbfb734f, before media_presentation.
INSERT INTO messages
		(id, chat_jid, sender, sender_server, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, quoted_message_id, direct_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, chat_jid) DO UPDATE SET
			sender = excluded.sender,
			-- Keep a namespace the row already has only while the user part it
			-- describes stays the same; a caller with a different bare sender
			-- and no namespace leaves it unknown rather than mislabelled.
			sender_server = CASE
				WHEN excluded.sender_server IS NOT NULL THEN excluded.sender_server
				WHEN excluded.sender = messages.sender THEN messages.sender_server
			END,
			-- Incomplete media stubs carry no replacement caption. Reaction
			-- removal still writes empty content; complete media may do so too.
			content = CASE WHEN NOT :complete_media AND excluded.content = '' AND messages.media_type IN
				('image', 'video', 'audio', 'document', 'sticker')
				THEN messages.content ELSE excluded.content END,
			-- The timestamp also names the cached file. Move it with the
			-- snapshot when a row can accept its first partial credentials.
			timestamp = CASE WHEN NOT (:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0)) AND messages.media_type IN
				('image', 'video', 'audio', 'document', 'sticker')
				THEN messages.timestamp ELSE excluded.timestamp END,
			is_from_me = excluded.is_from_me,
			media_type = CASE WHEN (:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0)) THEN COALESCE(NULLIF(excluded.media_type, ''), messages.media_type) ELSE messages.media_type END,
			filename = CASE WHEN (:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0)) THEN COALESCE(NULLIF(excluded.filename, ''), messages.filename) ELSE messages.filename END,
			url = CASE WHEN (:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0)) THEN excluded.url ELSE messages.url END,
			direct_path = CASE WHEN (:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0)) THEN excluded.direct_path ELSE messages.direct_path END,
			media_key = CASE WHEN (:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0)) THEN excluded.media_key ELSE messages.media_key END,
			file_sha256 = CASE WHEN (:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0)) THEN excluded.file_sha256 ELSE messages.file_sha256 END,
			file_enc_sha256 = CASE WHEN (:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0)) THEN excluded.file_enc_sha256 ELSE messages.file_enc_sha256 END,
			file_length = CASE WHEN (:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0)) THEN CASE WHEN excluded.file_length IS NULL AND excluded.file_sha256 = messages.file_sha256 THEN messages.file_length ELSE excluded.file_length END ELSE messages.file_length END,
			quoted_message_id = COALESCE(excluded.quoted_message_id, messages.quoted_message_id);
