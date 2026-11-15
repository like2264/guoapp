import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/foundation.dart';

class DiaryService {
  static final List<String> _entries = <String>[];
  static List<String> _restored = const <String>[];
  static const int maxEntries = 500;
  static String? nativeLogPath;
  static String _nativeText = '';
  static int _clears = 0;
  static final ValueNotifier<int> revision = ValueNotifier<int>(0);

  /// 应用自己的播放日志也落盘，否则杀掉进程重开就只剩「应用启动」一行，
  /// 真机反馈时完全看不出刚才发生了什么。
  static String? get _diaryPath {
    final path = nativeLogPath;
    if (path == null) return null;
    final index = path.lastIndexOf(RegExp(r'[\\/]'));
    if (index < 0) return 'diary.log';
    return '${path.substring(0, index)}/diary.log';
  }

  static void add(String message) {
    final entry = '[${_clock(DateTime.now())}] $message';
    _entries.add(entry);
    if (_entries.length > maxEntries) {
      _entries.removeRange(0, _entries.length - maxEntries);
    }
    revision.value++;
    unawaited(_persist(entry));
    debugPrint('[Diary] $message');
  }

  static Future<void> _persist(String entry) async {
    final path = _diaryPath;
    if (path == null) return;
    try {
      final file = File(path);
      if (!await file.parent.exists()) {
        await file.parent.create(recursive: true);
      }
      if (await file.exists() && await file.length() > 256 * 1024) {
        await file.writeAsString('');
      }
      await file.writeAsString('$entry\n', mode: FileMode.append);
    } on FileSystemException {
      return;
    }
  }

  static List<String> get entries => List<String>.unmodifiable([
    ..._restored,
    ..._entries,
    if (_nativeText.isNotEmpty) ..._nativeText.split('\n'),
  ]);

  static Future<void> clear() async {
    _entries.clear();
    _restored = const <String>[];
    _nativeText = '';
    _clears++;
    revision.value++;
    final diary = _diaryPath;
    if (diary != null) {
      for (final name in [diary, '$diary.1']) {
        try {
          await File(name).delete();
        } on FileSystemException {
          continue;
        }
      }
    }
    final path = nativeLogPath;
    if (path == null) return;
    for (final name in [path, '$path.1', '$path.2']) {
      try {
        await File(name).delete();
      } on FileSystemException {
        continue;
      }
    }
  }

  static String get fullText => [
    ..._restored,
    _entries.join('\n'),
    _nativeText,
  ].join('\n');

  static Future<void> refreshNativeLogs() async {
    final path = nativeLogPath;
    if (path == null) return;
    final clears = _clears;
    final lines = <String>[];
    for (final name in ['$path.1', path]) {
      RandomAccessFile? file;
      try {
        file = await File(name).open();
        final size = await file.length();
        final start = size > 128 * 1024 ? size - 128 * 1024 : 0;
        await file.setPosition(start);
        final text = utf8.decode(
          await file.read(size - start),
          allowMalformed: true,
        );
        for (final line in text.split('\n').skip(start > 0 ? 1 : 0)) {
          try {
            final event = jsonDecode(line) as Map<String, dynamic>;
            final name = event['event'] as String? ?? '';
            if (name.startsWith('stream.')) {
              lines.add(_nativeLine(event, name.substring('stream.'.length)));
            }
          } catch (_) {}
        }
      } on FileSystemException {
        continue;
      } finally {
        await file?.close();
      }
    }
    if (clears != _clears) return;
    _nativeText = lines
        .skip(lines.length > 500 ? lines.length - 500 : 0)
        .join('\n');
    // 上一次运行（进程被杀掉之前）的播放日志也读回来，方便真机反馈时有上下文。
    final diary = _diaryPath;
    if (diary == null) return;
    final restored = <String>[];
    RandomAccessFile? file;
    try {
      file = await File(diary).open();
      final size = await file.length();
      final start = size > 256 * 1024 ? size - 256 * 1024 : 0;
      await file.setPosition(start);
      final text = utf8.decode(
        await file.read(size - start),
        allowMalformed: true,
      );
      for (final line in text.split('\n').skip(start > 0 ? 1 : 0)) {
        if (line.trim().isNotEmpty) restored.add(line);
      }
    } on FileSystemException {
      return;
    } finally {
      await file?.close();
    }
    final current = _entries.toSet();
    _restored = restored
        .where((line) => !current.contains(line))
        .toList(growable: false);
  }

  static String _nativeLine(Map<String, dynamic> event, String name) {
    final time = DateTime.parse(event['time'] as String).toLocal();
    final status = event['httpStatus'];
    return [
      '[${_clock(time)}] [Stream] $name',
      event['host'],
      if (status != null) 'HTTP $status',
      event['responseType'],
      event['message'],
    ].whereType<String>().where((part) => part.isNotEmpty).join(' ');
  }

  static String _clock(DateTime time) =>
      '${time.hour.toString().padLeft(2, '0')}:${time.minute.toString().padLeft(2, '0')}:${time.second.toString().padLeft(2, '0')}.${time.millisecond ~/ 100}';
}
