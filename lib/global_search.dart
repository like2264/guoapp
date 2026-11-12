import 'dart:math' as math;

import 'core_bridge.dart';
import 'models.dart';

class GlobalSearch {
  GlobalSearch(this.repository, this.sources);
  final AppRepository repository;
  final List<SourceSite> sources;

  static const _concurrency = 3;

  final _more = <String, bool>{};
  final _seen = <String>{};
  String _query = '';

  Future<CatalogPage> search(String query, {int page = 1}) async {
    if (query != _query) {
      _query = query;
      _more.clear();
      _seen.clear();
    }
    final active = [
      for (final site in sources)
        if (_more[site.id] != false) site,
    ];
    if (active.isEmpty) return CatalogPage(const [], page: page);
    final pages = <String, CatalogPage>{};
    final failures = <String>[];
    for (var start = 0; start < active.length; start += _concurrency) {
      final batch = active.sublist(
        start,
        math.min(start + _concurrency, active.length),
      );
      await Future.wait([
        for (final site in batch)
          repository.catalog(site.id, query: query, page: page).then<void>(
            (value) {
              pages[site.id] = value;
            },
            onError: (Object error) {
              failures.add(error.toString());
            },
          ),
      ]);
    }
    if (pages.isEmpty) {
      throw AppFailure(failures.isEmpty ? '搜索失败' : failures.first);
    }
    final items = <Drama>[];
    var hasMore = false;
    for (final site in sources) {
      final result = pages[site.id];
      if (result == null) continue;
      final more = site.pagedSearch && result.hasMore;
      _more[site.id] = more;
      hasMore = hasMore || more;
      for (final drama in result.items) {
        if (_seen.add(drama.id)) items.add(drama);
      }
    }
    return CatalogPage(items, hasMore: hasMore, page: page);
  }
}
