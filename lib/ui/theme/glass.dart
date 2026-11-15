import 'dart:ui';

import 'package:flutter/material.dart';

/// Liquid Glass 的基础容器：背景模糊 + 极薄的高光层 + 细高光边。
///
/// 主题只能改颜色，模糊必须靠 [BackdropFilter]，所以玻璃效果统一走这个组件，
/// 需要“玻璃感”的地方直接包一层即可。默认几乎透明，让底下的封面/内容透出来；
/// 需要更实的底（例如深色页面上放正文）就把 [tint] 调大。
class GlassPanel extends StatelessWidget {
  const GlassPanel({
    super.key,
    required this.child,
    this.padding = const EdgeInsets.all(16),
    this.blur = 22,
    this.radius = 24,
    this.tint = 1,
    this.border = true,
  });

  final Widget child;
  final EdgeInsetsGeometry padding;
  final double blur;
  final double radius;

  /// 底色浓度倍率：1 是默认的极薄高光，调到 2~3 会明显更「实」。
  final double tint;

  /// 是否描一圈高光边（贴边使用的底栏一般不要）。
  final bool border;

  @override
  Widget build(BuildContext context) {
    final dark = Theme.of(context).brightness == Brightness.dark;
    final corners = BorderRadius.circular(radius);
    double alpha(double value) => (value * tint).clamp(0, 1).toDouble();
    return ClipRRect(
      borderRadius: corners,
      child: BackdropFilter(
        filter: ImageFilter.blur(sigmaX: blur, sigmaY: blur),
        child: DecoratedBox(
          decoration: BoxDecoration(
            borderRadius: corners,
            border: border
                ? Border.all(
                    color: dark
                        ? Colors.white.withValues(alpha: 0.16)
                        : Colors.white.withValues(alpha: 0.5),
                  )
                : null,
            gradient: LinearGradient(
              begin: Alignment.topLeft,
              end: Alignment.bottomRight,
              colors: dark
                  ? [
                      Colors.white.withValues(alpha: alpha(0.09)),
                      Colors.white.withValues(alpha: alpha(0.03)),
                    ]
                  : [
                      Colors.white.withValues(alpha: alpha(0.30)),
                      Colors.white.withValues(alpha: alpha(0.12)),
                    ],
            ),
          ),
          child: Padding(padding: padding, child: child),
        ),
      ),
    );
  }
}
