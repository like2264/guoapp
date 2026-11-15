import 'dart:async';
import 'dart:io';
import 'dart:ui' show Size;

import 'package:flutter/foundation.dart';
import 'package:media_kit/media_kit.dart';

import 'core_bridge.dart';
import 'danmaku_controller.dart';
import 'danmaku_style.dart';
import 'diary_service.dart';
import 'local_store.dart';
import 'models.dart';
import 'playback_loader.dart';
import 'playback_preferences.dart';
import 'playback_preloader.dart';
import 'playback_recovery.dart';

Size? videoDisplaySize(VideoParams parameters) {
  final width = (parameters.dw ?? 0) > 0 ? parameters.dw! : parameters.w ?? 0;
  final height = (parameters.dh ?? 0) > 0 ? parameters.dh! : parameters.h ?? 0;
  if (width <= 0 || height <= 0) return null;
  final rotation = (parameters.rotate ?? 0) % 180;
  return rotation == 90
      ? Size(height.toDouble(), width.toDouble())
      : Size(width.toDouble(), height.toDouble());
}

String _diagnostic(Object error) {
  final text = error.toString().replaceAll(
    RegExp(r'[a-z]+://\S+', caseSensitive: false),
    '[地址]',
  );
  return text.length > 300 ? text.substring(0, 300) : text;
}

class PlaybackSession extends ChangeNotifier {
  PlaybackSession({
    required this.repository,
    required this.store,
    required this.detail,
    required this._player,
    this._standby,
    required int index,
    double position = 0,
    bool autoplay = true,
  }) : _engaged = autoplay,
       _index = index,
       _epoch = store.profileEpoch {
    final preferences = store.playbackPreferences;
    _speed = preferences.speed;
    _quality = preferences.quality;
    _danmakuEnabled = preferences.danmaku;
    _unmutedVolume = store.playerVolume.clamp(1.0, 100.0);
    unawaited(_player.setVolume(_unmutedVolume));
    _loader = PlaybackLoader(repository);
    _preloader = PlaybackPreloader(repository)..addListener(_prime);
    danmaku = DanmakuController(repository)
      ..setEnabled(_danmakuEnabled)
      ..setBlockedWords(preferences.danmakuWords);
    store.addListener(_accessChanged);
    store.addListener(_syncBlockedWords);
    _bind();
    _saveTimer = Timer.periodic(
      const Duration(seconds: 5),
      (_) => saveProgress(quiet: true),
    );
    _healthTimer = Timer.periodic(const Duration(seconds: 1), (_) {
      if (_closed || !_acceptErrors || _loading || _error != null) return;
      if (_health.stalled(
        position: player.state.position,
        playing: player.state.playing && _playIntent,
        foreground: _foreground,
        now: DateTime.now(),
      )) {
        unawaited(_recover('position_stalled_40s'));
      }
    });
    unawaited(play(index, position: position, playWhenReady: autoplay));
  }

  void _bind() {
    for (final subscription in _subscriptions) {
      unawaited(subscription.cancel());
    }
    _subscriptions.clear();
    final size = videoDisplaySize(player.state.videoParams);
    if (size != null) {
      _aspectRatio = size.width / size.height;
      _aspectKnown = true;
    }
    _buffering(player.state.buffering);
    final stream = player.stream;
    _subscriptions.addAll([
      stream.error.listen(_playerError),
      stream.completed.listen(_completed),
      stream.position.listen((position) {
        if (!_closed && _openedIndex == _index && position > Duration.zero) {
          _resumePosition = position.inMilliseconds / 1000;
        }
        _trackPlayed(position);
        _sync();
      }),
      stream.playing.listen((playing) {
        if (!playing) endBoost();
        _sync();
      }),
      stream.buffering.listen((buffering) {
        _buffering(buffering);
        _sync();
      }),
      for (final changes in [stream.duration, stream.buffer, stream.rate])
        changes.listen((_) => _sync()),
      stream.videoParams.listen((parameters) {
        final size = videoDisplaySize(parameters);
        if (size == null || _closed) return;
        _aspectRatio = size.width / size.height;
        _aspectKnown = true;
        _notify();
      }),
    ]);
  }

  final AppRepository repository;
  final LocalStore store;
  final DramaDetail detail;
  Player _player;
  Player? _standby;
  late final DanmakuController danmaku;
  late final PlaybackLoader _loader;
  late final PlaybackPreloader _preloader;
  final _recovery = PlaybackRecovery();
  final _health = PlaybackHealth();
  final int _epoch;
  final List<StreamSubscription<dynamic>> _subscriptions = [];
  Timer? _saveTimer;
  Timer? _healthTimer;
  Timer? _errorTimer;
  Timer? _boostTimer;
  Timer? _stallTimer;
  Timer? _volumeTimer;
  Future<void> _operations = Future<void>.value();
  Future<void> _rates = Future<void>.value();
  int _index;
  int _openedIndex = -1;
  int _generation = 0;
  int _seekSequence = 0;
  bool _loading = true;
  bool _online = false;
  bool _localFailure = false;
  bool _loginRequired = false;
  bool _closed = false;
  bool _acceptErrors = false;
  bool _foreground = true;
  bool _playIntent = true;
  bool _pendingError = false;
  bool _boosting = false;
  bool _boostLocked = false;
  bool _engaged;
  bool _staleAudio = false;
  bool _stalled = false;
  String _loadingMessage = '正在准备播放';
  String? _error;
  String? _saveWarning;
  String? _savedProgressKey;
  bool _savedQuietly = false;
  PlaybackPlan? _plan;
  PlaybackPlan? _standbyPlan;
  int _standbyIndex = -1;
  bool _standbyOpen = false;
  DateTime _standbyAt = DateTime(2000);
  double _openedAt = 0;
  double _speed = 1;
  int _quality = 0;
  bool _danmakuEnabled = true;
  double _aspectRatio = 9 / 16;
  bool _aspectKnown = false;
  double _resumePosition = 0;
  double _unmutedVolume = 100;
  // 拖动进度后播放器有可能把「分片还没到」当成片尾：记下这次拖动，
  // 结束事件落在窗口内就不能当成真的播完（否则会静默跳下一集）。
  DateTime? _seekedAt;
  Duration? _seekTarget;
  double? _recoverPosition;
  // 光看「位置≈时长」判断不出真假结束：播放器把「分片没到」当成片尾时，
  // 会直接把位置钳到时长再报结束，表面和真播完一模一样。真正能分辨的是
  // 「这一段到底播了多久」——真播完时它几乎等于剩余时长，假结束时差得远。
  double _playedSeconds = 0;
  double _sessionStart = 0;
  Duration? _lastTick;
  Duration? _lastGoodPosition;

  Player get player => _player;

  Player? get standby => _standbyOpen ? _standby : null;
  int get standbyIndex => _standbyOpen ? _standbyIndex : -1;
  int get index => _index;
  Episode get episode => detail.episodes[_index];
  bool get loading => _loading;
  String get loadingMessage => _loadingMessage;
  String? get error => _error;
  bool get localFailure => _localFailure;
  bool get loginRequired => _loginRequired;
  String? get saveWarning => _saveWarning;
  bool get ready => !_loading && _error == null;

  bool get stalled => _stalled;

  bool get paused => !_playIntent;
  bool get boosting => _boosting;
  bool get boostLocked => _boostLocked;
  double get aspectRatio => _aspectRatio;
  bool get aspectKnown => _aspectKnown;
  double get speed => _speed;
  int get quality => _quality;
  bool get local => _plan?.local == true;
  List<int> get qualities => _plan?.qualities ?? const [];
  bool get danmakuSupported => detail.drama.source == SourceSite.hongguo.id;
  bool get danmakuEnabled => _danmakuEnabled;
  PlaybackPreferences get preferences => _preferences;
  DanmakuStyle get danmakuStyle => DanmakuStyle.of(store.playbackPreferences);
  bool get hasPrevious => _index > 0;
  bool get hasNext => _index + 1 < detail.episodes.length;
  String get qualityLabel {
    final plan = _plan;
    if (plan?.local == true) return '本地原画';
    if (_quality > 0) return '${_quality}P';
    return plan != null && plan.quality > 0 ? '自动（${plan.quality}P）' : '自动';
  }

  double get currentPosition =>
      _openedIndex == _index && player.state.position > Duration.zero
      ? player.state.position.inMilliseconds / 1000
      : _resumePosition;

  bool get _usable => !_closed && store.profileEpoch == _epoch;
  bool _current(int ticket) => _usable && ticket == _generation;

  PlaybackPreferences get _preferences => store.playbackPreferences.copyWith(
    speed: _speed,
    quality: _quality,
    danmaku: _danmakuEnabled,
    autoAdvance: true,
    preload: true,
  );

  double get _rate => _boosting ? 2 : _speed;

  void _buffering(bool buffering) {
    _stallTimer?.cancel();
    if (buffering) {
      _stallTimer = Timer(const Duration(milliseconds: 500), () {
        if (_closed || !player.state.buffering) return;
        _stalled = true;
        _notify();
      });
    } else if (_stalled) {
      _stalled = false;
      _notify();
    }
  }

  void _notify() {
    if (!_closed) notifyListeners();
  }

  void _accessChanged() {
    if (_closed) return;
    if (store.profileEpoch != _epoch || store.locked) {
      _preloader.clear();
      _dropStandby();
      _playIntent = false;
      endBoost();
      unawaited(player.pause());
    }
    if (store.profileEpoch != _epoch ||
        store.locked ||
        !store.allowsSource(SourceSite.hongguo.id)) {
      danmaku.setPlan(null);
    }
  }

  void _syncBlockedWords() {
    if (_closed) return;
    danmaku.setBlockedWords(store.playbackPreferences.danmakuWords);
  }

  void _sync() {
    _syncDanmaku();
    _syncPreload();
  }

  void _syncDanmaku() {
    if (_closed) return;
    final state = player.state;
    danmaku.update(
      position: state.position,
      duration: state.duration,
      speed: state.rate,
      playing: state.playing && _playIntent && !state.completed,
      buffering: state.buffering,
      foreground: _foreground,
      available:
          ready &&
          _openedIndex == _index &&
          _usable &&
          !store.locked &&
          store.allowsSource(SourceSite.hongguo.id),
    );
  }

  void _syncPreload() {
    if (_closed) return;
    if (!_foreground ||
        _plan?.local == true ||
        !ready ||
        _openedIndex != _index ||
        !_usable ||
        !_engaged ||
        store.locked ||
        !hasNext) {
      _preloader.clear();
      return;
    }
    final state = player.state;
    final duration = state.duration.inMilliseconds;
    final position = state.position.inMilliseconds;
    final ahead = state.buffer.inMilliseconds - position;
    if (_standby != null) {
      if (_standbyOpen &&
          DateTime.now().difference(_standbyAt) > const Duration(minutes: 8)) {
        _dropStandby();
      }
      if (_standbyIndex >= 0 || duration <= 0 || ahead < 3000) return;
    } else {
      if (!state.playing || state.buffering || !_playIntent) {
        _preloader.pause();
        return;
      }
      if (duration <= 0 ||
          position < 2000 ||
          (position < duration ~/ 2 && duration - position > 45000) ||
          ahead < 5000) {
        return;
      }
    }
    _preloader.prepare(
      detail.drama,
      detail.episodes[_index + 1],
      quality: _quality,
      online: _online,
    );
  }

  void _playerError(String error) {
    if (_closed || !_acceptErrors || error.trim().isEmpty) return;
    DiaryService.add('[Play] 播放器报错：${_diagnostic(error)}');
    _queueRecovery('player_error');
  }

  /// 累计「真正播过」的秒数：只认正常推进（0 < 增量 <= 4 秒）。
  /// 拖进度、跳集、以及播放器把位置钳到片尾都会造成大跳，一律不记；
  /// 顺便把最后一个正常位置留下来，假结束时当断点用。
  void _trackPlayed(Duration position) {
    final last = _lastTick;
    _lastTick = position;
    if (last == null) return;
    final delta = position - last;
    if (delta <= Duration.zero || delta > const Duration(seconds: 4)) return;
    _playedSeconds += delta.inMilliseconds / 1000;
    _lastGoodPosition = position;
  }

  /// 一次播放会话重新计数：换集、重新解析、拖动之后都从头算，
  /// 这样「实际播了多久」对得上「这一段本来该有多长」。
  void _resetPlayed(double start) {
    _playedSeconds = 0;
    _sessionStart = start;
    _lastTick = null;
    _lastGoodPosition = null;
  }

  void _completed(bool completed) {
    if (!completed || _loading || _closed || !_acceptErrors || _error != null) {
      return;
    }
    final duration = player.state.duration;
    final position = player.state.position;
    final seconds = duration.inMilliseconds / 1000;
    final played = _playedSeconds;
    DiaryService.add(
      '[Play] 播放器报告结束 位置=${position.inSeconds}s 时长=${duration.inSeconds}s 实际播放=${played.toStringAsFixed(0)}s 有下集=$hasNext',
    );
    final seekedAt = _seekedAt;
    final target = _seekTarget;
    final recentSeek =
        seekedAt != null &&
        DateTime.now().difference(seekedAt) < const Duration(seconds: 25);
    // 用户自己把进度拖到片尾附近，就是主动看完，按正常结束处理。
    final skippedToEnd =
        recentSeek &&
        target != null &&
        duration > Duration.zero &&
        target >= duration - const Duration(seconds: 12);
    if (!skippedToEnd) {
      // 拖到白色缓存线之外时，播放器取不到分片会直接报 EOF。落到「拖动后
      // 25 秒内、且不是拖到片尾」这种结束，先按拖动目标重新解析，不跳集。
      if (recentSeek &&
          (duration <= Duration.zero ||
              target == null ||
              target < duration - const Duration(seconds: 12))) {
        _seekedAt = null;
        _seekTarget = null;
        _recoverPosition =
            (target ?? Duration(milliseconds: position.inMilliseconds))
                .inMilliseconds /
            Duration.millisecondsPerSecond;
        DiaryService.add(
          '[Play] 拖动后提前结束，改从 ${_recoverPosition!.toStringAsFixed(1)}s 重新解析，不跳集',
        );
        _queueRecovery('seek_eof');
        return;
      }
      // 分片迟迟不到时播放器也会报结束：它把位置直接钳到时长，看起来和
      // 真播完一模一样。区别在于这一段实际只播了一小会儿，远没到剩余时长。
      final remaining = seconds - _sessionStart;
      if (seconds > 0 &&
          remaining > 8 &&
          played < remaining - 8 &&
          position >= duration - const Duration(seconds: 3)) {
        final lastGood =
            _lastGoodPosition ??
            Duration(
              milliseconds: (_sessionStart * Duration.millisecondsPerSecond)
                  .round(),
            );
        // 往回退 2 秒：出错的那一片往往就在断点处，重解析时让它重新取一次。
        final back = lastGood - const Duration(seconds: 2);
        _recoverPosition =
            (back > Duration.zero ? back : Duration.zero).inMilliseconds /
            Duration.millisecondsPerSecond;
        _seekedAt = null;
        _seekTarget = null;
        DiaryService.add(
          '[Play] 播放器把分片没到当成片尾（实际只播了 ${played.toStringAsFixed(0)}s，本段应有 ${remaining.toStringAsFixed(0)}s），改从 ${_recoverPosition!.toStringAsFixed(1)}s 继续，不跳集',
        );
        _queueRecovery('truncated_eof');
        return;
      }
    }
    if (duration <= Duration.zero ||
        position < duration - const Duration(seconds: 2)) {
      _queueRecovery('early_completion');
    } else if (_foreground && hasNext) {
      unawaited(play(_index + 1));
    } else {
      _playIntent = false;
      endBoost();
      unawaited(player.pause());
      unawaited(saveProgress());
    }
  }

  void _queueRecovery(String reason) {
    if (_closed || !_acceptErrors || _error != null) return;
    _pendingError = true;
    if (!_foreground || (_errorTimer?.isActive ?? false)) return;
    final ticket = _generation;
    final position = player.state.position;
    _errorTimer = Timer(const Duration(milliseconds: 900), () {
      if (!_current(ticket) || !_foreground || !_acceptErrors) return;
      _pendingError = false;
      final state = player.state;
      if (state.playing &&
          !state.buffering &&
          (state.width ?? 0) > 0 &&
          state.position > position + const Duration(milliseconds: 300)) {
        return;
      }
      unawaited(_recover(reason));
    });
  }

  Future<void> _recover(String reason) async {
    final current = _plan;
    if (_closed ||
        !_acceptErrors ||
        !_foreground ||
        current == null ||
        _error != null) {
      return;
    }
    _acceptErrors = false;
    danmaku.setPlan(null);
    _errorTimer?.cancel();
    _pendingError = false;
    final position = _recoverPosition ?? currentPosition;
    _recoverPosition = null;
    final action = current.local
        ? PlaybackRecoveryAction.stop
        : _recovery.next(current);
    DiaryService.add(
      '[Play] 自动恢复 原因=$reason 处理=${action.name} 位置=${position.toStringAsFixed(1)}s 线路=${current.routeIndex + 1}/${current.routeCount}',
    );
    if (action != PlaybackRecoveryAction.stop) {
      await play(_index, position: position, recovery: action);
      return;
    }
    _resumePosition = position;
    final ticket = _generation;
    try {
      await _serialize(() async {
        if (!_current(ticket)) return;
        try {
          await saveProgress();
          _openedIndex = -1;
          await player.stop();
        } finally {
          await _release(current.session);
        }
      });
    } catch (_) {}
    if (!_current(ticket)) return;
    _loading = false;
    _localFailure = current.local;
    _loginRequired = false;
    _error = current.local
        ? '本地视频读取失败，请重试或重新下载；也可以改为在线播放。'
        : '自动恢复未成功，请检查网络后重试，也可换一集或选择其他清晰度。';
    _notify();
  }

  Future<void> _release(String session) async {
    if (session.isEmpty) return;
    await repository.release(session).catchError((Object _) {});
  }

  Future<void> _serialize(Future<void> Function() operation) {
    final next = _operations.catchError((Object _) {}).then((_) => operation());
    _operations = next;
    return next;
  }

  Future<void> _configure(Player target, PlaybackPlan plan) async {
    final platform = target.platform;
    if (platform is! NativePlayer) return;
    final hwdec = switch (store.playbackPreferences.televisionDecoder) {
      TelevisionDecoder.software => 'no',
      TelevisionDecoder.hardware when Platform.isAndroid => 'mediacodec-copy',
      TelevisionDecoder.hardware when Platform.isIOS || Platform.isMacOS =>
        'videotoolbox-copy',
      _ when Platform.isWindows => 'd3d11va-copy',
      _ when Platform.isAndroid => 'auto-safe',
      _ when Platform.isMacOS => 'auto-safe',
      _ => null,
    };
    if (hwdec != null) await platform.setProperty('hwdec', hwdec);
    if (Platform.isAndroid) {
      await platform.setProperty('hwdec-codecs', 'all');
      await platform.setProperty('vd-lavc-skiploopfilter', 'nonkey');
      await platform.setProperty('vd-lavc-threads', '4');
      await platform.setProperty('vd-lavc-fast', 'yes');
      await platform.setProperty('video-sync', 'audio');
      await platform.setProperty('framedrop', 'vo');
    }
    await platform.setProperty(
      'demuxer-lavf-o',
      [
        'seg_max_retry=3',
        'strict=experimental',
        'allowed_extensions=ALL',
        plan.local
            ? 'protocol_whitelist=[file,crypto,data]'
            : 'protocol_whitelist=[http,https,tcp,tls,crypto,data,file]',
        if (plan.decryptionKey.isNotEmpty)
          'decryption_key=${plan.decryptionKey}',
      ].join(','),
    );
    // 本机流服务会先自己扛住上游抖动（空闲 30 秒才掐断、失败还会重试一次），
    // 播放器这边的超时必须比它宽，否则分片还在重试就被判成 EOF、整集结束。
    await platform.setProperty('network-timeout', '45');
  }

  Future<void> play(
    int index, {
    double position = 0,
    PlaybackRecoveryAction? recovery,
    bool playWhenReady = true,
  }) async {
    if (!_usable || index < 0 || index >= detail.episodes.length) return;
    endBoost();
    _staleAudio = false;
    if (index != _index) _online = false;
    if (index != _index) {
      // 换集后上一次拖动的记录不能留着，否则会挡住新一集的正常结束。
      _seekedAt = null;
      _seekTarget = null;
      _recoverPosition = null;
    }
    if (recovery == null &&
        position == 0 &&
        _standbyOpen &&
        _standbyIndex == index &&
        !_online) {
      return _swap(index, playWhenReady);
    }
    _dropStandby();
    final warmed = recovery == null
        ? _preloader.take(
            detail.drama,
            detail.episodes[index],
            quality: _quality,
            online: _online,
          )
        : null;
    _preloader.clear();
    final ticket = ++_generation;
    _seekSequence++;
    danmaku.setPlan(null);
    _acceptErrors = false;
    _pendingError = false;
    _errorTimer?.cancel();
    _health.reset();
    if (recovery == null) {
      _recovery.reset();
      _playIntent = playWhenReady;
    }
    _resumePosition = position;
    _openedAt = position;
    _resetPlayed(position);
    _index = index;
    _loading = true;
    _error = null;
    _localFailure = false;
    _loginRequired = false;
    _loadingMessage = switch (recovery) {
      PlaybackRecoveryAction.alternative => '正在切换备用线路',
      PlaybackRecoveryAction.refresh => '正在重新获取播放地址',
      _ => '正在准备播放',
    };
    _notify();
    PlaybackPlan? prepared;
    PlaybackPlan? retained;
    var installed = false;
    try {
      await _serialize(() async {
        if (!_current(ticket)) return;
        await saveProgress();
        if (!_current(ticket)) return;
        _openedIndex = -1;
        await player.stop();
        final previous = _plan;
        _plan = null;
        if (recovery == PlaybackRecoveryAction.alternative) {
          retained = previous;
        } else if (previous != null) {
          await _release(previous.session);
        }
      });
      if (!_current(ticket)) return;
      prepared = retained != null
          ? await _loader.fallback(retained!)
          : warmed != null
          ? await _loader.use(warmed)
          : await _loader.load(
              detail.drama,
              detail.episodes[index],
              quality: _quality,
              online: _online,
            );
      if (prepared == null) return;
      final plan = prepared;
      await _serialize(() async {
        if (!_current(ticket)) {
          await _release(plan.session);
          return;
        }
        if (plan.url.isEmpty) throw AppFailure('站源未返回播放地址，请重试');
        await _configure(player, plan);
        await _limitCache(player, !_engaged);
        await _selectAudio(player, true);
        _plan = plan;
        installed = true;
        _acceptErrors = true;
        await player.open(
          Media(
            plan.url,
            httpHeaders: plan.headers,
            start: position > 0
                ? Duration(milliseconds: (position * 1000).round())
                : null,
          ),
          play: _foreground && _playIntent,
        );
        if (!_current(ticket)) return;
        _openedIndex = index;
        _health.reset();
        await _setRate(_rate);
        if (!_current(ticket)) return;
        _loading = false;
        _notify();
        danmaku.setPlan(plan);
        _syncDanmaku();
      });
    } catch (error) {
      if (_current(ticket)) {
        if (prepared != null && identical(_plan, prepared)) {
          _acceptErrors = true;
          _queueRecovery('open_exception');
        } else {
          if (prepared != null) await _release(prepared.session);
          if (_current(ticket)) {
            DiaryService.add('[Play] 无法播放：${_diagnostic(error)}');
            _loading = false;
            _localFailure = error is AppFailure && error.code == 'local_media';
            _loginRequired =
                error is AppFailure && error.code == 'source_login';
            _error = error is AppFailure ? error.message : '无法播放这一集，请重试或换一集。';
            _notify();
          }
        }
      } else if (prepared != null && !installed) {
        await _release(prepared.session);
      }
    } finally {
      if (warmed != null && !installed) await _release(warmed.session);
      if (retained != null) await _release(retained!.session);
    }
  }

  Future<void> _swap(int index, bool playWhenReady) async {
    final plan = _standbyPlan!;
    final next = _standby!;
    final previous = _player;
    final previousPlan = _plan;
    final volume = previous.state.volume;
    unawaited(saveProgress());
    _preloader.clear();
    final ticket = ++_generation;
    _seekSequence++;
    _pendingError = false;
    _errorTimer?.cancel();
    _health.reset();
    _recovery.reset();
    _playIntent = playWhenReady;
    _resumePosition = 0;
    _openedAt = 0;
    _index = index;
    _error = null;
    _localFailure = false;
    _loginRequired = false;
    _loading = false;
    _standbyPlan = null;
    _standbyIndex = -1;
    _standbyOpen = false;
    _player = next;
    _standby = previous;
    _plan = plan;
    _bind();
    _openedIndex = index;
    _acceptErrors = true;
    final stopping = previous.stop().catchError((Object _) {});
    if (previousPlan != null) unawaited(_release(previousPlan.session));
    danmaku.setPlan(plan);
    _notify();
    _syncDanmaku();
    try {
      await _limitCache(next, false);
      await next.setVolume(volume);
      await _setRate(_rate);
      await stopping;
      await _selectAudio(next, true);
      if (_current(ticket) && _foreground && _playIntent) await next.play();
    } catch (_) {
      if (_current(ticket)) _queueRecovery('open_exception');
    }
  }

  void _prime() {
    final standby = _standby;
    final next = _index + 1;
    if (_closed ||
        standby == null ||
        _standbyIndex >= 0 ||
        !_preloader.ready ||
        next >= detail.episodes.length) {
      return;
    }
    _standbyIndex = next;
    final plan = _preloader.take(
      detail.drama,
      detail.episodes[next],
      quality: _quality,
      online: _online,
    );
    if (plan == null) {
      _standbyIndex = -1;
      return;
    }
    _standbyPlan = plan;
    unawaited(_open(standby, plan));
  }

  Future<void> _open(Player standby, PlaybackPlan plan) async {
    try {
      await _configure(standby, plan);
      await _limitCache(standby, true);
      await _selectAudio(standby, false);
      await standby.open(
        Media(plan.url, httpHeaders: plan.headers),
        play: false,
      );
    } catch (_) {
      if (identical(_standbyPlan, plan)) _dropStandby();
      return;
    }
    if (_closed || !identical(_standbyPlan, plan)) return;
    _standbyOpen = true;
    _standbyAt = DateTime.now();
    _notify();
  }

  void _dropStandby() {
    final plan = _standbyPlan;
    final opened = _standbyOpen || plan != null;
    _standbyPlan = null;
    _standbyIndex = -1;
    _standbyOpen = false;
    if (opened) unawaited(_standby?.stop().catchError((Object _) {}));
    if (plan != null) unawaited(_release(plan.session));
  }

  Future<void> _limitCache(Player target, bool limited) async {
    final platform = target.platform;
    if (platform is! NativePlayer) return;
    await platform.setProperty(
      'demuxer-max-bytes',
      '${(limited ? 4 : 32) * 1024 * 1024}',
    );
  }

  Future<void> _selectAudio(Player target, bool enabled) async {
    final platform = target.platform;
    if (platform is! NativePlayer || !Platform.isIOS) return;
    await platform.setProperty('aid', enabled ? 'auto' : 'no');
  }

  void start(int index, {double position = 0}) {
    if (!_usable) return;
    final wasEngaged = _engaged;
    _engaged = true;
    if (index != _index ||
        (position - _openedAt).abs() > .5 ||
        _error != null) {
      unawaited(play(index, position: position));
      return;
    }
    _playIntent = true;
    if (!wasEngaged) unawaited(_limitCache(player, false));
    if (ready) unawaited(player.play());
  }

  Future<void> retry() =>
      play(_index, position: currentPosition, playWhenReady: true);

  Future<void> switchOnline() {
    _online = true;
    return retry();
  }

  Future<bool> next() async {
    if (!hasNext) return false;
    await play(_index + 1);
    return true;
  }

  Future<bool> previous() async {
    if (!hasPrevious) return false;
    await play(_index - 1);
    return true;
  }

  void togglePlayback() {
    if (!_usable || !ready) return;
    endBoost();
    _playIntent = !player.state.playing;
    _health.reset();
    if (_playIntent && player.state.completed) {
      unawaited(play(_index));
      return;
    }
    if (_playIntent && _staleAudio) {
      _staleAudio = false;
      unawaited(player.seek(player.state.position).then((_) => player.play()));
    } else {
      unawaited(player.playOrPause());
    }
    if (!_playIntent) unawaited(saveProgress());
    _syncDanmaku();
  }

  Future<bool> seekTo(Duration target) async {
    if (!_usable || !ready) return false;
    final ticket = ++_seekSequence;
    final generation = _generation;
    danmaku.beginSeek();
    _seekedAt = DateTime.now();
    _seekTarget = target;
    _resetPlayed(target.inMilliseconds / Duration.millisecondsPerSecond);
    _lastGoodPosition = target;
    var succeeded = false;
    try {
      await player.seek(target);
      succeeded = true;
    } catch (_) {
    } finally {
      if (!_closed && ticket == _seekSequence && generation == _generation) {
        danmaku.endSeek(succeeded ? target : player.state.position);
      }
    }
    return succeeded;
  }

  Duration? seekBy(int seconds) {
    final duration = player.state.duration;
    if (!ready || duration <= Duration.zero) return null;
    final target = Duration(
      milliseconds: (player.state.position.inMilliseconds + seconds * 1000)
          .clamp(0, duration.inMilliseconds),
    );
    unawaited(seekTo(target));
    return target;
  }

  Future<void> _setRate(double value) {
    _rates = _rates
        .catchError((Object _) {})
        .then((_) => player.setRate(value));
    return _rates.catchError((Object _) {});
  }

  void beginBoost() {
    if (!ready || _boostTimer != null || _boosting) return;
    _boostTimer = Timer(const Duration(milliseconds: 350), () {
      _boostTimer = null;
      if (_closed ||
          !ready ||
          !player.state.playing ||
          player.state.completed) {
        return;
      }
      _boosting = true;
      unawaited(_setRate(2));
      _notify();
    });
  }

  void holdBoost() {
    if (!_usable || _boosting) return;
    _boostTimer?.cancel();
    _boostTimer = null;
    _boosting = true;
    unawaited(_setRate(2));
    _notify();
  }

  void setBoostLocked(bool locked) {
    if (_boostLocked == locked) return;
    if (locked) {
      holdBoost();
      _boostLocked = true;
      _notify();
    } else {
      _boostLocked = false;
      endBoost();
      _notify();
    }
  }

  bool endBoost() {
    if (_boostLocked) return false;
    final boosted = _boosting;
    _boostTimer?.cancel();
    _boostTimer = null;
    _boosting = false;
    if (boosted) {
      unawaited(_setRate(_speed));
      _notify();
    }
    return boosted;
  }

  void setVolume(double value) {
    final volume = value.clamp(0.0, 100.0);
    if (volume > 0) {
      _unmutedVolume = volume;
      _volumeTimer?.cancel();
      _volumeTimer = Timer(const Duration(milliseconds: 500), _saveVolume);
    }
    _applyVolume(volume);
  }

  void _saveVolume() {
    _volumeTimer = null;
    unawaited(store.setPlayerVolume(_unmutedVolume).catchError((Object _) {}));
  }

  void _applyVolume(double volume) {
    if (!player.state.playing) _staleAudio = true;
    unawaited(player.setVolume(volume));
  }

  double changeVolume(double delta) {
    final volume = (player.state.volume + delta).clamp(0.0, 100.0);
    setVolume(volume);
    return volume;
  }

  double toggleMute() {
    final current = player.state.volume;
    if (current > 0) _unmutedVolume = current;
    final target = current > 0 ? 0.0 : _unmutedVolume;
    _applyVolume(target);
    return target;
  }

  Future<void> _savePreferences(PlaybackPreferences preferences) async {
    if (!_usable) throw StateError('当前用户已变更');
    await store.setPlaybackPreferences(preferences);
    if (!_usable) throw StateError('当前用户已变更');
  }

  Future<void> setSpeed(double value) async {
    if (value == _speed) return;
    await _savePreferences(_preferences.copyWith(speed: value));
    _speed = value;
    _notify();
    if (!_boosting) await _setRate(value);
  }

  Future<void> setQuality(int value) async {
    if (value == _quality) return;
    await _savePreferences(_preferences.copyWith(quality: value));
    _quality = value;
    _preloader.clear();
    _notify();
    if (_plan?.local == true) return;
    await play(
      _index,
      position: currentPosition,
      playWhenReady: _error != null || player.state.playing,
    );
  }

  Future<void> toggleDanmaku() async {
    if (!danmakuSupported) return;
    final enabled = !_danmakuEnabled;
    await _savePreferences(_preferences.copyWith(danmaku: enabled));
    _danmakuEnabled = enabled;
    danmaku.setEnabled(enabled);
    _syncDanmaku();
    _notify();
  }

  Future<void> setDanmakuScale(double value) async {
    final scale = value.clamp(danmakuScaleMin, danmakuScaleMax);
    await _savePreferences(_preferences.copyWith(danmakuScale: scale));
    _notify();
  }

  Future<void> setDanmakuColor(int value) async {
    await _savePreferences(_preferences.copyWith(danmakuColor: value));
    _notify();
  }

  Future<void> setDanmakuOpacity(double value) async {
    final opacity = value.clamp(.1, 1.0);
    await _savePreferences(_preferences.copyWith(danmakuOpacity: opacity));
    _notify();
  }

  Future<void> setDanmakuBold(bool value) async {
    await _savePreferences(_preferences.copyWith(danmakuBold: value));
    _notify();
  }

  Future<void> setDanmakuStroke(double value) async {
    final stroke = value.clamp(danmakuStrokeMin, danmakuStrokeMax);
    await _savePreferences(_preferences.copyWith(danmakuStroke: stroke));
    _notify();
  }

  Future<void> setDanmakuShadow(double value) async {
    final shadow = value.clamp(danmakuShadowMin, danmakuShadowMax);
    await _savePreferences(_preferences.copyWith(danmakuShadow: shadow));
    _notify();
  }

  Future<void> setDanmakuCoverage(DanmakuCoverage value) async {
    await _savePreferences(_preferences.copyWith(danmakuCoverage: value));
    _notify();
  }

  Future<void> setDanmakuWords(List<String> words) async {
    final normalized = normalizeDanmakuWords(words);
    await _savePreferences(_preferences.copyWith(danmakuWords: normalized));
    _notify();
  }

  void setForeground(bool visible) {
    if (_closed || _foreground == visible) return;
    _foreground = visible;
    _health.reset();
    _sync();
    if (!visible) {
      endBoost();
      _playIntent = false;
      unawaited(player.pause());
      unawaited(saveProgress());
    } else if (_pendingError) {
      _queueRecovery('foreground_pending_error');
    }
  }

  Future<void> saveProgress({bool quiet = false}) async {
    if (!_engaged || _openedIndex < 0 || store.profileEpoch != _epoch) return;
    final position = player.state.position.inMilliseconds / 1000;
    final duration = player.state.duration.inMilliseconds / 1000;
    if (position < .1) return;
    final key = '${detail.drama.id}:$_openedIndex:$position:$duration';
    if (_savedProgressKey == key &&
        _saveWarning == null &&
        (quiet || !_savedQuietly)) {
      return;
    }
    final entry = WatchEntry(
      drama: repository.catalogUpdates.current(detail.drama),
      episode: detail.episodes[_openedIndex].number,
      position: position,
      duration: duration,
      updatedAt: DateTime.now(),
    );
    try {
      await store.saveWatch(entry, quiet: quiet);
      _savedProgressKey = key;
      _savedQuietly = quiet;
      if (_saveWarning != null) {
        _saveWarning = null;
        _notify();
      }
    } catch (_) {
      if (_saveWarning == null) {
        _saveWarning = '观看进度尚未保存，请检查存储空间后重试。';
        _notify();
      }
    }
  }

  @override
  void dispose() {
    unawaited(saveProgress());
    _closed = true;
    _generation++;
    store.removeListener(_accessChanged);
    store.removeListener(_syncBlockedWords);
    _saveTimer?.cancel();
    _healthTimer?.cancel();
    _errorTimer?.cancel();
    _boostTimer?.cancel();
    _stallTimer?.cancel();
    _volumeTimer?.cancel();
    if (_volumeTimer != null) _saveVolume();
    for (final subscription in _subscriptions) {
      unawaited(subscription.cancel());
    }
    danmaku.dispose();
    _preloader.dispose();
    final session = _plan?.session ?? '';
    unawaited(_release(session));
    final standbyPlan = _standbyPlan;
    if (standbyPlan != null) unawaited(_release(standbyPlan.session));
    final standby = _standby;
    unawaited(_loader.close().catchError((Object _) {}));
    unawaited(
      _operations.catchError((Object _) {}).then((_) async {
        await _rates.catchError((Object _) {});
        await player.dispose();
        await standby?.dispose();
      }),
    );
    super.dispose();
  }
}
