import 'models.dart';

enum PlaybackRecoveryAction { alternative, refresh, stop }

class PlaybackRecovery {
  // 站点侧分片要跨网取，拖到未缓冲位置时首片常常要几秒到十几秒；
  // 之前 3 次就收手，慢网络下会把还能救的播放直接判死（黑屏）。
  static const maxAttempts = 4;
  int _attempts = 0;
  bool _refreshed = false;

  void reset() {
    _attempts = 0;
    _refreshed = false;
  }

  PlaybackRecoveryAction next(PlaybackPlan plan) {
    if (_attempts >= maxAttempts) {
      return PlaybackRecoveryAction.stop;
    }
    if (plan.hasAlternative) {
      _attempts++;
      return PlaybackRecoveryAction.alternative;
    }
    if (!_refreshed) {
      _attempts++;
      _refreshed = true;
      return PlaybackRecoveryAction.refresh;
    }
    return PlaybackRecoveryAction.stop;
  }
}

class PlaybackHealth {
  // 20 秒对跨网站源太紧：拖进度后首片就要 5~15 秒，还没到数据就把会话判死，
  // 于是「卡一下 → 跳下一集 / 黑屏」。
  static const stallTimeout = Duration(seconds: 40);
  Duration? _position;
  DateTime? _lastProgress;

  void reset() {
    _position = null;
    _lastProgress = null;
  }

  bool stalled({
    required Duration position,
    required bool playing,
    required bool foreground,
    required DateTime now,
  }) {
    if (!playing || !foreground || position != _position) {
      _position = position;
      _lastProgress = now;
      return false;
    }
    _lastProgress ??= now;
    return now.difference(_lastProgress!) >= stallTimeout;
  }
}
